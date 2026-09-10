package worker

import (
	"bytes"
	"encoding/binary"
	"hash/fnv"
	"io"
	"os"
	"sync"
)

const (
	bloomFilterBytes          = 1024            // 每个分块布隆过滤器占 1024 字节 (8192 bits)
	bloomChunkLines           = 15000           // 每个分块约 15000 行
	bloomMagic                = "BIDX0002"      // 8 字节文件魔数 (版本 2)
	minFileSizeBytesForBloom  = 1 * 1024 * 1024 // 1MB 以上的文件开启布隆索引
)

// BloomFilter 8192-bit 高效布隆过滤器
type BloomFilter [bloomFilterBytes]byte

func hashWord(word []byte) (uint32, uint32) {
	// 使用 FNV-1a 计算基础散列，并构造双重散列 (Kirsch-Mitzenmacher 算法)
	h := fnv.New32a()
	_, _ = h.Write(word)
	h1 := h.Sum32()

	// 辅助散列
	h2 := (h1 * 0x85ebca6b) ^ (h1 >> 13)
	if h2 == 0 {
		h2 = 0x5bd1e995
	}
	return h1, h2
}

// Add 将单词 Token 加入布隆过滤器 (4 个散列函数)
func (b *BloomFilter) Add(word []byte) {
	if len(word) == 0 || isLongPureDigits(word) {
		return
	}
	h1, h2 := hashWord(word)
	const numBits = uint32(bloomFilterBytes * 8)

	for i := uint32(0); i < 4; i++ {
		bitPos := (h1 + i*h2) % numBits
		b[bitPos/8] |= (1 << (bitPos % 8))
	}
}

// MayContain 判断目标词是否可能存在于该布隆过滤器中 (若返回 false 则绝对不存在)
func (b *BloomFilter) MayContain(word []byte) bool {
	if len(word) == 0 || isLongPureDigits(word) {
		return true
	}
	h1, h2 := hashWord(word)
	const numBits = uint32(bloomFilterBytes * 8)

	for i := uint32(0); i < 4; i++ {
		bitPos := (h1 + i*h2) % numBits
		if (b[bitPos/8] & (1 << (bitPos % 8))) == 0 {
			return false
		}
	}
	return true
}

func isLongPureDigits(word []byte) bool {
	if len(word) <= 4 {
		return false
	}
	for i := 0; i < len(word); i++ {
		if word[i] < '0' || word[i] > '9' {
			return false
		}
	}
	return true
}

// MayContainSearchKeyword 针对用户搜索关键字执行分词布隆判定
func (b *BloomFilter) MayContainSearchKeyword(kw []byte) bool {
	cleanKw := bytes.ToLower(bytes.TrimSpace(kw))
	if len(cleanKw) < 2 {
		return true
	}

	start := -1
	hasToken := false
	for i := 0; i < len(cleanKw); i++ {
		c := cleanKw[i]
		isChar := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-'
		if isChar {
			if start == -1 {
				start = i
			}
		} else {
			if start != -1 {
				tok := cleanKw[start:i]
				if len(tok) >= 2 && len(tok) <= 64 {
					hasToken = true
					if !b.MayContain(tok) {
						return false
					}
				}
				start = -1
			}
		}
	}
	if start != -1 {
		tok := cleanKw[start:]
		if len(tok) >= 2 && len(tok) <= 64 {
			hasToken = true
			if !b.MayContain(tok) {
				return false
			}
		}
	}

	if !hasToken && len(cleanKw) >= 2 && len(cleanKw) <= 64 {
		return b.MayContain(cleanKw)
	}
	return true
}

// BloomChunkMeta 单个分块的元数据及布隆位图
type BloomChunkMeta struct {
	ChunkIndex  int32
	StartLine   int64
	LineCount   int32
	StartOffset int64
	EndOffset   int64
	Filter      BloomFilter
}

// ChunkBloomIndex 整个文件的分块布隆索引
type ChunkBloomIndex struct {
	FileSize    int64
	ModTimeNano int64
	TotalLines  int64
	Chunks      []BloomChunkMeta
}

var (
	bloomBuildMu     sync.Mutex
	bloomMemoryCache = make(map[string]*ChunkBloomIndex)
	bloomCacheMu     sync.RWMutex
)

// GetOrBuildBloomIndex 获取或构建指定大文件的分块布隆索引
func GetOrBuildBloomIndex(filePath string) (*ChunkBloomIndex, error) {
	fi, err := os.Stat(filePath)
	if err != nil {
		return nil, err
	}

	if fi.Size() < minFileSizeBytesForBloom {
		return nil, nil // 小于 4MB 的小文件无需构建布隆索引
	}

	// 1. 检查内存缓存
	bloomCacheMu.RLock()
	idx, ok := bloomMemoryCache[filePath]
	bloomCacheMu.RUnlock()
	if ok && idx != nil && idx.FileSize == fi.Size() && idx.ModTimeNano == fi.ModTime().UnixNano() {
		return idx, nil
	}

	// 2. 检查磁盘持久化文件 (.bidx)
	bidxPath := filePath + ".bidx"
	if idx, err := loadBloomIndexFromDisk(bidxPath, fi.Size(), fi.ModTime().UnixNano()); err == nil && idx != nil {
		bloomCacheMu.Lock()
		bloomMemoryCache[filePath] = idx
		bloomCacheMu.Unlock()
		return idx, nil
	}

	// 3. 单飞构建防并发雪崩
	bloomBuildMu.Lock()
	defer bloomBuildMu.Unlock()

	// 双重检查
	bloomCacheMu.RLock()
	if idx, ok := bloomMemoryCache[filePath]; ok && idx != nil && idx.FileSize == fi.Size() && idx.ModTimeNano == fi.ModTime().UnixNano() {
		bloomCacheMu.RUnlock()
		return idx, nil
	}
	bloomCacheMu.RUnlock()

	idx, err = buildChunkBloomIndex(filePath, fi.Size(), fi.ModTime().UnixNano())
	if err != nil {
		return nil, err
	}

	// 异步固化到磁盘
	go func(p string, data *ChunkBloomIndex) {
		_ = saveBloomIndexToDisk(p, data)
	}(bidxPath, idx)

	bloomCacheMu.Lock()
	bloomMemoryCache[filePath] = idx
	bloomCacheMu.Unlock()

	return idx, nil
}

// buildChunkBloomIndex 扫描文件并构建分块布隆稀疏位图
func buildChunkBloomIndex(filePath string, fileSize, modTimeNano int64) (*ChunkBloomIndex, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	adviseSequential(f)

	idx := &ChunkBloomIndex{
		FileSize:    fileSize,
		ModTimeNano: modTimeNano,
		Chunks:      make([]BloomChunkMeta, 0, fileSize/(bloomChunkLines*100)+16),
	}

	buf := make([]byte, 1024*1024)
	var currentFileOffset int64 = 0
	var lineCount int64 = 0

	var currentChunk BloomChunkMeta
	currentChunk.ChunkIndex = 0
	currentChunk.StartLine = 1
	currentChunk.StartOffset = 0

	var chunkLineCount int32 = 0
	lowerTokenBuf := make([]byte, 128)

	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			chunkBytes := buf[:n]
			pos := 0
			for {
				newlineIdx := bytes.IndexByte(chunkBytes[pos:], '\n')
				if newlineIdx == -1 {
					// 行未结束，将本段内容提取 token 加入当前 chunk 的布隆过滤器
					extractTokensToBloom(&currentChunk.Filter, chunkBytes[pos:], lowerTokenBuf)
					break
				}

				lineSlice := chunkBytes[pos : pos+newlineIdx]
				extractTokensToBloom(&currentChunk.Filter, lineSlice, lowerTokenBuf)

				lineCount++
				chunkLineCount++
				absNewlineOffset := currentFileOffset + int64(pos+newlineIdx)

				if chunkLineCount >= bloomChunkLines {
					currentChunk.LineCount = chunkLineCount
					currentChunk.EndOffset = absNewlineOffset + 1
					idx.Chunks = append(idx.Chunks, currentChunk)

					// 开启下一个 Chunk
					currentChunk = BloomChunkMeta{
						ChunkIndex:  int32(len(idx.Chunks)),
						StartLine:   lineCount + 1,
						StartOffset: absNewlineOffset + 1,
					}
					chunkLineCount = 0
				}

				pos += newlineIdx + 1
			}
			currentFileOffset += int64(n)
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			return nil, rerr
		}
	}

	// 收尾最后一个 Chunk
	if chunkLineCount > 0 || len(idx.Chunks) == 0 {
		currentChunk.LineCount = chunkLineCount
		currentChunk.EndOffset = fileSize
		idx.Chunks = append(idx.Chunks, currentChunk)
	}

	idx.TotalLines = lineCount
	return idx, nil
}

// extractTokensToBloom 极速分词并将纯字母数字及常见标识符加入 BloomFilter
func extractTokensToBloom(bf *BloomFilter, data []byte, lowerBuf []byte) {
	start := -1
	for i := 0; i < len(data); i++ {
		b := data[i]
		// 允许 a-z, A-Z, 0-9, _, - 作为 token 字符
		isTokenChar := (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_' || b == '-'
		if isTokenChar {
			if start == -1 {
				start = i
			}
		} else {
			if start != -1 {
				tokenLen := i - start
				if tokenLen >= 2 && tokenLen <= 64 {
					token := data[start:i]
					toLowerASCII(token, lowerBuf[:tokenLen])
					bf.Add(lowerBuf[:tokenLen])
				}
				start = -1
			}
		}
	}
	if start != -1 {
		tokenLen := len(data) - start
		if tokenLen >= 2 && tokenLen <= 64 {
			token := data[start:]
			toLowerASCII(token, lowerBuf[:tokenLen])
			bf.Add(lowerBuf[:tokenLen])
		}
	}
}

func toLowerASCII(src, dst []byte) {
	for i := 0; i < len(src); i++ {
		c := src[i]
		if c >= 'A' && c <= 'Z' {
			dst[i] = c + ('a' - 'A')
		} else {
			dst[i] = c
		}
	}
}

// 磁盘持久化格式:
// Header (36 字节):
// [0..8]   Magic: "BIDX0001"
// [8..12]  ChunkCount (uint32)
// [12..20] TotalLines (int64)
// [20..28] FileSize (int64)
// [28..36] ModTimeNano (int64)
// 每个 Chunk (544 字节):
// ChunkIndex (int32 - 4)
// StartLine (int64 - 8)
// LineCount (int32 - 4)
// StartOffset (int64 - 8)
// EndOffset (int64 - 8)
// Filter ([512]byte)

func loadBloomIndexFromDisk(bidxPath string, expectedSize, expectedModTimeNano int64) (*ChunkBloomIndex, error) {
	f, err := os.Open(bidxPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	header := make([]byte, 36)
	if _, err := io.ReadFull(f, header); err != nil {
		return nil, err
	}

	if string(header[0:8]) != bloomMagic {
		return nil, os.ErrInvalid
	}

	chunkCount := binary.LittleEndian.Uint32(header[8:12])
	totalLines := int64(binary.LittleEndian.Uint64(header[12:20]))
	fileSize := int64(binary.LittleEndian.Uint64(header[20:28]))
	modTimeNano := int64(binary.LittleEndian.Uint64(header[28:36]))

	if fileSize != expectedSize || modTimeNano != expectedModTimeNano {
		return nil, os.ErrInvalid
	}

	idx := &ChunkBloomIndex{
		FileSize:    fileSize,
		ModTimeNano: modTimeNano,
		TotalLines:  totalLines,
		Chunks:      make([]BloomChunkMeta, chunkCount),
	}

	chunkRecordBuf := make([]byte, 32+bloomFilterBytes)
	for i := uint32(0); i < chunkCount; i++ {
		if _, err := io.ReadFull(f, chunkRecordBuf); err != nil {
			return nil, err
		}
		c := &idx.Chunks[i]
		c.ChunkIndex = int32(binary.LittleEndian.Uint32(chunkRecordBuf[0:4]))
		c.StartLine = int64(binary.LittleEndian.Uint64(chunkRecordBuf[4:12]))
		c.LineCount = int32(binary.LittleEndian.Uint32(chunkRecordBuf[12:16]))
		c.StartOffset = int64(binary.LittleEndian.Uint64(chunkRecordBuf[16:24]))
		c.EndOffset = int64(binary.LittleEndian.Uint64(chunkRecordBuf[24:32]))
		copy(c.Filter[:], chunkRecordBuf[32:32+bloomFilterBytes])
	}

	return idx, nil
}

func saveBloomIndexToDisk(bidxPath string, idx *ChunkBloomIndex) error {
	tmpPath := bidxPath + ".tmp"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}

	header := make([]byte, 36)
	copy(header[0:8], bloomMagic)
	binary.LittleEndian.PutUint32(header[8:12], uint32(len(idx.Chunks)))
	binary.LittleEndian.PutUint64(header[12:20], uint64(idx.TotalLines))
	binary.LittleEndian.PutUint64(header[20:28], uint64(idx.FileSize))
	binary.LittleEndian.PutUint64(header[28:36], uint64(idx.ModTimeNano))

	if _, err := f.Write(header); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return err
	}

	recordBuf := make([]byte, 32+bloomFilterBytes)
	for i := range idx.Chunks {
		c := &idx.Chunks[i]
		binary.LittleEndian.PutUint32(recordBuf[0:4], uint32(c.ChunkIndex))
		binary.LittleEndian.PutUint64(recordBuf[4:12], uint64(c.StartLine))
		binary.LittleEndian.PutUint32(recordBuf[12:16], uint32(c.LineCount))
		binary.LittleEndian.PutUint64(recordBuf[16:24], uint64(c.StartOffset))
		binary.LittleEndian.PutUint64(recordBuf[24:32], uint64(c.EndOffset))
		copy(recordBuf[32:32+bloomFilterBytes], c.Filter[:])

		if _, err := f.Write(recordBuf); err != nil {
			_ = f.Close()
			_ = os.Remove(tmpPath)
			return err
		}
	}

	_ = f.Close()
	return os.Rename(tmpPath, bidxPath)
}
