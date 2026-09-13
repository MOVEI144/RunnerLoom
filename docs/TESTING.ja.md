# テスト方針

このページは「どのテストが何を証明するか」です。
合格済みの実行番号は [VERIFICATION.md](VERIFICATION.md)、v1でまだ足りない証拠は [V1_ACCEPTANCE.md](V1_ACCEPTANCE.md) です。

PRを出す手順は [CONTRIBUTING.md](../CONTRIBUTING.md) です。

## 迷ったらこの表

| 知りたいこと | 使うもの | それで分かること |
|---|---|---|
| この変更が既存の契約を壊していないか | `make check` | 書式、vet、race付きのリポジトリテスト |
| GitHubに出したPRが同じ検査を通るか | RunnerLoom CI の `checks` | 上に加え fuzz、`.deb` 梱包、`govulncheck` |
| 実Ubuntu VMが起動・作業・削除されるか | CI の `real-vm`、または `runnerloom smoke-vm` | GitHub-hosted の使い捨てホスト上の1台ライフサイクル |
| Golden Imageが署名検証付きで作れ、bootするか | CI の `golden-image` | Canonical署名、Runner digest、mask方針、boot後のRunner有無 |
| ControllerとAgentが本物のTLSで同期するか | `internal/control` の統合テスト | 実TLS / HTTP / SQLite。GitHubとhypervisorは明示的なfake |
| 2台の実VMと所有確認付きcleanupか | `e2e` qualification（`main` のみ） | 実libvirt。GitHub需要はfixture。自宅クラスタではない |
| 文書のリンクと見出しが壊れていないか | `python3.12 scripts/check-docs.py` | ネットワークなしのローカル検査 |
| 実GitHub Jobが1件完走するか | [V1_ACCEPTANCE.md](V1_ACCEPTANCE.md) の live workflow | 対象デプロイの証拠。CIの緑だけでは代替できない |

## ローカル

```bash
make check
```

中身は次です。

```text
gofmt
go vet ./...
go test -race -count=1 ./...
bash -n  （image-build / ci-vm-smoke / ci-free-space）
```

梱包を確認するときだけ:

```bash
make package
```

実ホストでVMの作成から削除まで見るときだけ、自分のマシンで:

```bash
runnerloom smoke-vm
```

`internal/e2e` は `//go:build linux` です。Linux以外ではそのパッケージはビルドされません。

## CIジョブ

製品CIは次だけです。`.github/workflows/internal-*` は一時作業用の残骸で、合格条件ではありません。

| ワークフロー | ジョブ | 動くとき | 証明すること | 証明しないこと |
|---|---|---|---|---|
| RunnerLoom CI | Go, race, integration and CLI（`checks`） | `main` へのpush、PR、手動 | ユニット、競合、mTLS統合、fuzz、Debian梱包、出荷binaryの脆弱性スキャン | 実KVM、実GitHub Job |
| RunnerLoom CI | Real Ubuntu VM lifecycle | `checks` の後。同一RepositoryのPRと `main` | 署名済みベース、boot、ゲスト作業、ホストprobe拒否、poweroff、所有ディスク削除 | Golden Imageの公式Runner入り、複数Node |
| RunnerLoom CI | Build verified runner image and boot it | `checks` の後。同一RepositoryのPRと `main` | 未登録Golden Imageのbuild、service mask、Runner binary、そのイメージでのsmoke | 自宅ホストの容量やLAN隔離キャンペーン |
| RunnerLoom runtime qualification | Build image, enroll Node, run two REAL VMs… | `main` へのpushと手動 | 実libvirtで2 VM、enroll、cleanup | ライブGitHub API、自宅Node |
| RunnerLoom runtime qualification | Live GitHub prerequisites ONLY | 上と同じ | 専用secretがあるかどうかの記録 | Jobの実行そのもの |
| RunnerLoom documentation | Reader paths and local links | 文書変更時 | 手順書のリンク・見出し | 設計の正しさ |
| RunnerLoom v1 live acceptance | 手動 | workflow_dispatch | 許可した非公開Repositoryでの実Job | CIの代替 |

今のCIランナーと製品ホストは Ubuntu 24.04 x86_64 です。macOS / Windows ホストは将来対象にする可能性があり、今は資格がありません。CIでも macOS / Windows ランナーは使いません。

## パッケージが守ること

| 場所 | 主に守ること |
|---|---|
| `internal/core` | 設定、資源台帳、予約、配置理由、SQLite、JIT暗号化、CA/招待 |
| `internal/control` | Nodeプロトコル。管理APIを載せない。実TLS + fake GitHub/hypervisor |
| `internal/agent` | 外向きmTLS、ensure/stop/delete、image取得 |
| `internal/github` | 公式 `actions/scaleset`、ACK前のpersist、redirect禁止 |
| `internal/host` | libvirt所有権、専用network、cache pruneのfail-closed、download再開 |
| `internal/cli` | 人間向けCLIと `--json`、対話はTTYだけ、秘密を出さない |
| `internal/discovery` | Avahiは未信頼ヒント |
| `internal/e2e` | 実libvirt。GitHub側は診断fixture |

`control` の統合テストを「実VMテスト」と呼ばないでください。実VMは `real-vm`、`golden-image`、`e2e`、`smoke-vm` です。

## 緑でも足りないこと

次はCIが緑でも証明しません。

- 2台の物理LAN Node
- 公開fork由来のJob
- GPU、Windows/macOSホスト、Controller HA
- 完全なbackup/restoreと旧Controllerのfencing
- ストレージ圧やpeer-VM隔離の実機キャンペーン

これらは [V1_ACCEPTANCE.md](V1_ACCEPTANCE.md) のデプロイ証拠です。
