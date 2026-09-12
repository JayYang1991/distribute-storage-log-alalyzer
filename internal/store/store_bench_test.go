package store

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"time"

	"dist-log-analyzer/internal/model"
)

func generateBenchmarkAlarms(count int) []*model.Alarm {
	res := make([]*model.Alarm, count)
	now := time.Now()
	for i := 0; i < count; i++ {
		offset := time.Duration(rand.Intn(100000)) * time.Second
		res[i] = &model.Alarm{
			ID:          fmt.Sprintf("alm_%d", i),
			LastOccurAt: now.Add(-offset),
			Title:       fmt.Sprintf("Test alarm %d", i),
		}
	}
	return res
}

func BenchmarkAlarmSort_1_LegacyBubble(b *testing.B) {
	alarms := generateBenchmarkAlarms(1000)
	work := make([]*model.Alarm, len(alarms))

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		copy(work, alarms)
		// 旧逻辑：O(N^2) 冒泡排序
		for m := 0; m < len(work)-1; m++ {
			for n := m + 1; n < len(work); n++ {
				if work[m].LastOccurAt.Before(work[n].LastOccurAt) {
					work[m], work[n] = work[n], work[m]
				}
			}
		}
	}
}

func BenchmarkAlarmSort_2_OptimizedQuick(b *testing.B) {
	alarms := generateBenchmarkAlarms(1000)
	work := make([]*model.Alarm, len(alarms))

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		copy(work, alarms)
		// 新逻辑：O(N log N) 快速排序
		sort.Slice(work, func(m, n int) bool {
			return work[m].LastOccurAt.After(work[n].LastOccurAt)
		})
	}
}
