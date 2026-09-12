package worker

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"sync"

	lru "github.com/hashicorp/golang-lru/v2"
)

const (
	indexStep                = 5000            // 每 5000 行记录一个文件字节偏移量
	indexMagic               = "LIDX0001"      // 8 字节文件魔数
	minFileSizeBytesForIndex = 4 * 1024 * 1024 // 4MB 以上的文件开启稀疏行号索引
)

type SparseLineIndex struct {
	Step        uint32
	TotalLines  int64
	Offsets     []int64 // Offsets[k] 对应行号 (1 + k * Step) 的字节偏移
	FileSize    int64
	ModTimeNano int64
}

const numIndexStripes = 64

type indexStripeLock struct {
	locks [numIndexStripes]sync.Mutex
}

func (s *indexStripeLock) getLock(key string) *sync.Mutex {
	var h uint32
	for i := 0; i < len(key); i++ {
		h = 31*h + uint32(key[i])
	}
	return &s.locks[h%numIndexStripes]
}

var (
	indexStripes  indexStripeLock
	indexLRUCache *lru.Cache[string, *SparseLineIndex]
	indexLRUOnce  sync.Once
)

func getIndexLRUCache() *lru.Cache[string, *SparseLineIndex] {
	indexLRUOnce.Do(func() {
		cache, err := lru.New[string, *SparseLineIndex](1000)
		if err == nil {
			indexLRUCache = cache
		}
	})
	return indexLRUCache
}

// GetOrBuildLineIndex 获取或构建指定文件的稀疏行号索引 (支持多文件并发构建，带 LRU 容量上限)
func GetOrBuildLineIndex(filePath string) (*SparseLineIndex, error) {
	fi, err := os.Stat(filePath)
	if err != nil {
		return nil, err
	}

	if fi.Size() < minFileSizeBytesForIndex {
		return nil, nil // 小于阈值无需构建索引
	}

	cache := getIndexLRUCache()

	// 1. 检查内存 LRU 缓存
	if cache != nil {
		if idx, ok := cache.Get(filePath); ok && idx != nil && idx.FileSize == fi.Size() && idx.ModTimeNano == fi.ModTime().UnixNano() {
			return idx, nil
		}
	}

	// 2. 检查磁盘索引文件 (.lidx)
	idxPath := filePath + ".lidx"
	if idx, err := loadLineIndexFromDisk(idxPath, fi.Size(), fi.ModTime().UnixNano()); err == nil && idx != nil {
		if cache != nil {
			cache.Add(filePath, idx)
		}
		return idx, nil
	}

	// 3. 条带化细粒度单飞构建，允许不同文件并行构建，同文件并发防重
	lock := indexStripes.getLock(filePath)
	lock.Lock()
	defer lock.Unlock()

	// 双重检查
	if cache != nil {
		if idx, ok := cache.Get(filePath); ok && idx != nil && idx.FileSize == fi.Size() && idx.ModTimeNano == fi.ModTime().UnixNano() {
			return idx, nil
		}
	}

	idx, err := buildSparseLineIndex(filePath, fi.Size(), fi.ModTime().UnixNano())
	if err != nil {
		return nil, err
	}

	// 异步持久化到磁盘
	go func(p string, data *SparseLineIndex) {
		_ = saveLineIndexToDisk(p, data)
	}(idxPath, idx)

	if cache != nil {
		cache.Add(filePath, idx)
	}

	return idx, nil
}

// buildSparseLineIndex 极速扫描文件并建立稀疏索引
func buildSparseLineIndex(filePath string, fileSize, modTimeNano int64) (*SparseLineIndex, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	adviseSequential(f)

	idx := &SparseLineIndex{
		Step:        indexStep,
		FileSize:    fileSize,
		ModTimeNano: modTimeNano,
		Offsets:     []int64{0}, // 第 1 行偏移始终为 0
	}

	buf := make([]byte, 1024*1024)
	var currentFileOffset int64 = 0
	var lineCount int64 = 0

	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			pos := 0
			for {
				newlineIdx := bytes.IndexByte(chunk[pos:], '\n')
				if newlineIdx == -1 {
					break
				}
				lineCount++
				absNewlineOffset := currentFileOffset + int64(pos+newlineIdx)
				if lineCount%int64(idx.Step) == 0 {
					// 记录下一个字符（即下一行的起始位置）作为偏移
					idx.Offsets = append(idx.Offsets, absNewlineOffset+1)
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

	idx.TotalLines = lineCount
	return idx, nil
}

func loadLineIndexFromDisk(idxPath string, expectedSize, expectedModTimeNano int64) (*SparseLineIndex, error) {
	f, err := os.Open(idxPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	header := make([]byte, 36)
	if _, err := io.ReadFull(f, header); err != nil {
		return nil, err
	}

	if string(header[0:8]) != indexMagic {
		return nil, os.ErrInvalid
	}

	step := binary.LittleEndian.Uint32(header[8:12])
	totalLines := int64(binary.LittleEndian.Uint64(header[12:20]))
	fileSize := int64(binary.LittleEndian.Uint64(header[20:28]))
	modTimeNano := int64(binary.LittleEndian.Uint64(header[28:36]))

	if fileSize != expectedSize || modTimeNano != expectedModTimeNano {
		return nil, os.ErrInvalid
	}

	fi, _ := f.Stat()
	payloadSize := fi.Size() - 36
	if payloadSize%8 != 0 {
		return nil, os.ErrInvalid
	}

	count := payloadSize / 8
	offsets := make([]int64, count)
	if err := binary.Read(f, binary.LittleEndian, &offsets); err != nil {
		return nil, err
	}

	return &SparseLineIndex{
		Step:        step,
		TotalLines:  totalLines,
		Offsets:     offsets,
		FileSize:    fileSize,
		ModTimeNano: modTimeNano,
	}, nil
}

func saveLineIndexToDisk(idxPath string, idx *SparseLineIndex) error {
	tmpPath := idxPath + ".tmp"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	bw := bufio.NewWriterSize(f, 64*1024)

	header := make([]byte, 36)
	copy(header[0:8], indexMagic)
	binary.LittleEndian.PutUint32(header[8:12], idx.Step)
	binary.LittleEndian.PutUint64(header[12:20], uint64(idx.TotalLines))
	binary.LittleEndian.PutUint64(header[20:28], uint64(idx.FileSize))
	binary.LittleEndian.PutUint64(header[28:36], uint64(idx.ModTimeNano))

	if _, err := bw.Write(header); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}

	if err := binary.Write(bw, binary.LittleEndian, idx.Offsets); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}

	if err := bw.Flush(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}

	_ = f.Close()
	return os.Rename(tmpPath, idxPath)
}

// ReadFileLinesWithIndex 结合稀疏行号索引，实现任意行号秒级直达读取
func ReadFileLinesWithIndex(filePath string, startLine, limit int) (lines []string, hasMore bool, totalLines int64, err error) {
	if limit <= 0 {
		limit = 500
	}
	if limit > 5000 {
		limit = 5000
	}
	if startLine <= 0 {
		startLine = 1
	}

	f, err := os.Open(filePath)
	if err != nil {
		return nil, false, 0, err
	}
	defer f.Close()

	var seekOffset int64 = 0
	currentLine := 0

	// 尝试获取稀疏索引快速定位
	idx, _ := GetOrBuildLineIndex(filePath)
	if idx != nil {
		totalLines = idx.TotalLines
		if startLine > 1 && len(idx.Offsets) > 0 {
			bucket := (startLine - 1) / int(idx.Step)
			if bucket >= len(idx.Offsets) {
				bucket = len(idx.Offsets) - 1
			}
			if bucket >= 0 {
				seekOffset = idx.Offsets[bucket]
				currentLine = bucket * int(idx.Step)
			}
		}
	}

	if seekOffset > 0 {
		if _, err := f.Seek(seekOffset, io.SeekStart); err != nil {
			// 若 seek 失败降级从头读取
			_, _ = f.Seek(0, io.SeekStart)
			currentLine = 0
		}
	}

	scanner := bufio.NewScanner(f)
	bufPtr := searchBufPool.Get().(*[]byte)
	defer searchBufPool.Put(bufPtr)
	scanner.Buffer(*bufPtr, 10*1024*1024)

	lines = make([]string, 0, limit)
	for scanner.Scan() {
		currentLine++
		if currentLine < startLine {
			continue
		}
		lines = append(lines, scanner.Text())
		if len(lines) >= limit {
			break
		}
	}

	if len(lines) >= limit && scanner.Scan() {
		hasMore = true
	}

	return lines, hasMore, totalLines, nil
}
