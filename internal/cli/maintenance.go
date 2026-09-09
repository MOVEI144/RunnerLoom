package cli

import (
	_ "embed"

	"github.com/MOVEI144/RunnerLoom/internal/core"
	"github.com/MOVEI144/RunnerLoom/internal/host"
	"github.com/spf13/cobra"
)

//go:embed cluster.schema.json
var clusterSchema []byte

func (a *App) addMaintenance(root *cobra.Command) {
	config, _, e := root.Find([]string{"config"})
	if e != nil {
		panic(e)
	}
	add(config, "schema", "エディター・AI向けのJSON Schemaを出力。資源整合性はvalidateで追加検査", 0, func(*cobra.Command, []string) error {
		_, e := a.Out.Write(clusterSchema)
		return e
	})
	network, _, e := root.Find([]string{"network"})
	if e != nil {
		panic(e)
	}
	add(network, "suggest", "既存のLAN・VPN経路と重ならないVM用サブネットを提案（変更なし）", 0, func(c *cobra.Command, _ []string) error {
		cidr, e := host.SuggestSubnet(c.Context(), host.SystemExecutor{})
		if e != nil {
			return e
		}
		return a.output(map[string]any{"networkCIDR": cidr, "applied": false, "next": "Node設定へ指定し、network plan/applyで再確認してください"})
	})
	doctor, _, e := root.Find([]string{"doctor"})
	if e != nil {
		panic(e)
	}
	var strict bool
	doctor.Flags().BoolVar(&strict, "strict", false, "実行Nodeに必要な機能が不足していると非ゼロで終了")
	doctor.RunE = func(*cobra.Command, []string) error {
		checks := core.Doctor()
		if strict {
			for _, c := range checks {
				if c.Status == "fail" {
					return core.Fail("HOST_NOT_READY", "実行Nodeの準備が完了していません。診断結果を確認してください", checks)
				}
			}
		}
		return a.output(checks)
	}
}
