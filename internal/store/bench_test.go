package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/mehrnet/radar-node/internal/wire"
)

func BenchmarkRecordResults(b *testing.B) {
	for _, batchSize := range []int{1, 50, 200} {
		b.Run(fmt.Sprintf("batch_%d", batchSize), func(b *testing.B) {
			local, err := Open(filepath.Join(b.TempDir(), "radar.db"), Options{Origin: "node_bench"})
			if err != nil {
				b.Fatalf("Open: %v", err)
			}
			defer local.Close()
			ctx := context.Background()
			now := time.Now().UTC()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				batch := make([]wire.Result, batchSize)
				for j := range batch {
					batch[j] = testResult(fmt.Sprintf("run_%d_%d", i, j), "probe_1", j+1, now)
				}
				if _, err := local.RecordResults(ctx, batch); err != nil {
					b.Fatalf("RecordResults: %v", err)
				}
			}
			b.StopTimer()
			stats, err := local.Stats(ctx)
			if err != nil {
				b.Fatalf("Stats: %v", err)
			}
			total := float64(b.N * batchSize)
			b.ReportMetric(total/float64(b.Elapsed().Seconds()), "results/s")
			b.ReportMetric(float64(stats.DBBytes+stats.WALBytes)/total, "db-bytes/result")
		})
	}
}
