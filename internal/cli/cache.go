package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/MOVEI144/RunnerLoom/internal/agent"
	"github.com/MOVEI144/RunnerLoom/internal/core"
	"github.com/MOVEI144/RunnerLoom/internal/host"
	"github.com/spf13/cobra"
)

func controllerCacheProtection(ctx context.Context, state string) (host.CacheProtection, error) {
	if _, err := os.Lstat(filepath.Join(state, "controller.db")); err != nil {
		if os.IsNotExist(err) {
			return host.CacheProtection{}, core.Fail("NOT_CONFIGURED", "Controller設定がありません", nil)
		}
		return host.CacheProtection{}, err
	}
	store, err := core.OpenStore(state)
	if err != nil {
		return host.CacheProtection{}, err
	}
	defer store.Close()
	config, revision, err := store.Config(ctx)
	if err != nil {
		return host.CacheProtection{}, err
	}
	if revision == 0 {
		return host.CacheProtection{}, core.Fail("NOT_CONFIGURED", "Controller設定がありません", nil)
	}
	protection := host.CacheProtection{Digests: map[string][]string{}, Safe: true}
	for _, pool := range config.Pools {
		if !pool.Enabled {
			continue
		}
		image, ok := config.Image(pool.Image)
		if !ok {
			return host.CacheProtection{}, errors.New("configured Pool references a missing image")
		}
		protection.Digests[image.Digest] = append(protection.Digests[image.Digest], "enabled-pool:"+pool.Name)
	}
	instances, err := store.Instances(ctx)
	if err != nil {
		return host.CacheProtection{}, err
	}
	for _, instance := range instances {
		if instance.State != "Deleted" {
			protection.Digests[instance.Image.Digest] = append(protection.Digests[instance.Image.Digest], "instance:"+instance.ID+":"+instance.State)
		}
	}
	return protection, nil
}

func controllerCacheSpec(state string, limitGiB int64, verify bool) host.ImageCacheSpec {
	return host.ImageCacheSpec{
		Scope:       "controller",
		StateDir:    state,
		CacheDir:    filepath.Join(state, "images"),
		ProcessLock: "controller",
		LimitGiB:    limitGiB,
		Exec:        host.SystemExecutor{},
		Verify:      verify,
		ProtectionFunc: func(ctx context.Context) (host.CacheProtection, error) {
			return controllerCacheProtection(ctx, state)
		},
	}
}

func nodeCacheSpec(configPath string, verify bool) (host.ImageCacheSpec, error) {
	config, err := agent.LoadConfig(configPath)
	if err != nil {
		return host.ImageCacheSpec{}, err
	}
	exec := host.SystemExecutor{}
	return host.ImageCacheSpec{
		Scope:       "node",
		StateDir:    config.StateDir,
		CacheDir:    filepath.Join(config.StateDir, "images"),
		DiskDir:     config.DiskDir,
		ProcessLock: "agent",
		LimitGiB:    config.CacheGiB,
		Exec:        exec,
		Verify:      verify,
		ProtectionFunc: func(ctx context.Context) (host.CacheProtection, error) {
			return host.NodeCacheProtection(ctx, config.StateDir, config.DiskDir, config.Cluster, config.Node, exec), nil
		},
	}, nil
}

func cacheSpec(state, configPath string, limitGiB int64, verify bool) (host.ImageCacheSpec, error) {
	if configPath != "" {
		return nodeCacheSpec(configPath, verify)
	}
	if limitGiB < 1 {
		return host.ImageCacheSpec{}, errors.New("controller cache limit must be positive")
	}
	return controllerCacheSpec(state, limitGiB, verify), nil
}

func (a *App) addCache(root *cobra.Command) {
	cache := &cobra.Command{Use: "cache", Short: "ControllerとNodeのImage cacheを点検・整理・重複排除"}
	root.AddCommand(cache)

	var statusConfig string
	var statusLimit int64
	var verify bool
	status := add(cache, "status", "容量・参照・partial・base linkを表示（変更なし）", 0, func(c *cobra.Command, _ []string) error {
		spec, err := cacheSpec(a.State, statusConfig, statusLimit, verify)
		if err != nil {
			return err
		}
		report, err := host.InspectImageCache(c.Context(), spec)
		if err != nil {
			return err
		}
		return a.output(report)
	})
	status.Flags().StringVar(&statusConfig, "config", "", "Nodeを点検する場合のagent.json。省略時はController cache")
	status.Flags().Int64Var(&statusLimit, "cache-gib", 100, "Controller cacheの運用上限。Nodeではagent.jsonのcacheGiBを使用")
	status.Flags().BoolVar(&verify, "verify", false, "全ImageのSHA-256とqcow2構造を読み直す")

	var pruneConfig string
	var pruneLimit int64
	var olderThan time.Duration
	var apply bool
	prune := add(cache, "prune", "参照されない古いImage/partialを整理。既定はdry-run", 0, func(c *cobra.Command, _ []string) error {
		spec, err := cacheSpec(a.State, pruneConfig, pruneLimit, false)
		if err != nil {
			return err
		}
		report, err := host.PruneImageCache(c.Context(), spec, olderThan, apply)
		if err != nil {
			return err
		}
		return a.output(report)
	})
	prune.Flags().StringVar(&pruneConfig, "config", "", "Nodeを整理する場合のagent.json。省略時はController cache")
	prune.Flags().Int64Var(&pruneLimit, "cache-gib", 100, "Controller cacheの運用上限。Nodeではagent.jsonのcacheGiBを使用")
	prune.Flags().DurationVar(&olderThan, "older-than", 7*24*time.Hour, "この期間より古く、参照されない項目だけを候補にする（24時間以上）")
	prune.Flags().BoolVar(&apply, "apply", false, "所有service停止後、再検査した候補だけを実際に削除")

	var seedConfig string
	var sourceState string
	seed := add(cache, "seed DIGEST", "同一HostのController cacheからNode cacheへhard link（Agent停止中）", 1, func(c *cobra.Command, args []string) error {
		spec, err := nodeCacheSpec(seedConfig, false)
		if err != nil {
			return err
		}
		report, err := host.SeedImageCache(c.Context(), filepath.Join(sourceState, "images"), spec, args[0])
		if err != nil {
			return err
		}
		return a.output(report)
	})
	seed.Flags().StringVar(&seedConfig, "config", "", "Nodeのagent.json")
	seed.Flags().StringVar(&sourceState, "source-state", "", "同一HostにあるController stateの絶対パス")
	_ = seed.MarkFlagRequired("config")
	_ = seed.MarkFlagRequired("source-state")
}
