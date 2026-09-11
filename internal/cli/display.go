package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/MOVEI144/RunnerLoom/internal/core"
	"github.com/MOVEI144/RunnerLoom/internal/host"
)

// Human-readable tables are deliberately separate from the stable JSON output.
// Every string is escaped because even a diagnostic log is untrusted input.
func humanOutput(w io.Writer, v any) (bool, error) {
	t := tabwriter.NewWriter(w, 0, 4, 3, ' ', 0)
	line := func(values ...any) {
		parts := make([]string, len(values))
		for i, v := range values {
			parts[i] = terminalSafeLog([]byte(fmt.Sprint(v)))
		}
		fmt.Fprintln(t, strings.Join(parts, "\t"))
	}
	switch rows := v.(type) {
	case []core.Check:
		line("項目", "状態", "説明")
		for _, r := range rows {
			line(r.Name, r.Status, r.Detail)
		}
	case []core.Pool:
		line("POOL", "GitHubで選ぶ名前", "vCPU", "RAM MiB", "DISK GiB", "最大台数", "有効")
		for _, p := range rows {
			line(p.Name, p.RunnerName, p.VCPU, p.MemoryMiB, p.RootGiB+p.ScratchGiB, p.MaxRunners, p.Enabled)
		}
		if len(rows) == 0 {
			line("Poolはまだ定義されていません。")
		}
	case []core.Candidate:
		line("NODE", "空きvCPU", "空きRAM MiB", "空きDISK GiB", "予約", "配置できない理由")
		for _, r := range rows {
			reason := strings.Join(r.Reasons, ", ")
			if reason == "" {
				reason = "配置可能"
			}
			line(r.Node, r.Available.CPU, r.Available.Memory, r.Available.Disk, r.Reservation, reason)
		}
	case []core.Instance:
		line("VM ID", "NODE", "POOL", "状態", "結果")
		for _, r := range rows {
			line(r.ID, r.Node, r.Pool.Name, r.State, r.Result)
		}
		if len(rows) == 0 {
			line("実行記録はまだありません。")
		}
	case host.ImageCacheReport:
		line("CACHE", rows.Scope, rows.CacheDir)
		line("使用量 bytes", rows.CacheBytes, "新規Imageに使用可能 bytes", rows.AvailableForNewImageBytes)
		line("KIND", "DIGEST", "BYTES", "保護", "検証", "BASE", "整理可否/理由")
		for _, entry := range rows.Entries {
			digest := entry.Digest
			if len(digest) > 22 {
				digest = digest[:19] + "..."
			}
			reason := strings.Join(entry.BlockedReasons, ",")
			if entry.Reclaimable {
				reason = "候補"
			} else if reason == "" {
				reason = "保留"
			}
			line(entry.Kind, digest, entry.SizeBytes, entry.Protected, entry.Verification, entry.BasePresent, reason)
		}
		for _, warning := range rows.Warnings {
			line("警告", warning)
		}
	case host.ImageCachePruneReport:
		line("CACHE整理", rows.Scope, "適用", rows.Applied, "基準", rows.Cutoff.Local().Format(time.RFC3339))
		line("削除path数", len(rows.Removed), "論理bytes", rows.RemovedLogicalBytes, "物理解放見込みbytes", rows.ReclaimedBytes)
		line("KIND", "DIGEST", "BYTES", "保護", "整理可否/理由")
		for _, entry := range rows.Entries {
			digest := entry.Digest
			if len(digest) > 22 {
				digest = digest[:19] + "..."
			}
			reason := strings.Join(entry.BlockedReasons, ",")
			if entry.Reclaimable {
				reason = "削除候補"
			}
			line(entry.Kind, digest, entry.SizeBytes, entry.Protected, reason)
		}
	case host.ImageCacheSeedReport:
		line("DIGEST", "BYTES", "重複排除", "既存", "保存先")
		line(rows.Digest, rows.SizeBytes, rows.Deduplicated, rows.AlreadyThere, rows.Destination)
	case map[string]core.Observation:
		line("NODE", "受付状態", "最後の通信", "VM数")
		names := make([]string, 0, len(rows))
		for k := range rows {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, name := range names {
			r := rows[name]
			status := "準備未完了"
			if r.Ready {
				status = "受付可能"
			}
			if r.Drained {
				status = "受付停止"
			}
			if time.Since(r.Seen) > 45*time.Second {
				status = "通信確認待ち（資源保持）"
			}
			line(name, status, r.Seen.Local().Format(time.RFC3339), len(r.Instances))
		}
	default:
		return false, nil
	}
	return true, t.Flush()
}
