# RunnerLoom

自宅・社内の **Ubuntuマシン** を、GitHub Actions向けの使い捨てVM Runner群として使うための基盤です。Jobごとに新しいVMを作成し、終了後はホスト側で停止・削除を確認してから資源を解放します。

> Run each GitHub Actions job in a fresh VM on your own Ubuntu machines.

[Releases](https://github.com/MOVEI144/RunnerLoom/releases) · [ドキュメント一覧](docs/README.md) · [1台構成セットアップ](docs/QUICKSTART.ja.md) · [Security](SECURITY.md) · [検証済み範囲](docs/VERIFICATION.md)

> [!IMPORTANT]
> 現在はリリース候補です。対象は **Ubuntu 24.04 x86_64 / CPU VM / 信頼できる管理者 / 明示的に許可した非公開Repository** です。GPU passthrough、Windows/macOSホスト、Controller HA、公開fork由来のJobは対象外です。

## 迷ったらここから

| やりたいこと | 読むページ |
|---|---|
| まず何をするソフトか知りたい | このREADMEの「仕組み」 |
| CLIをインストール・更新・削除したい | [INSTALL.md](docs/INSTALL.md) |
| 1台のPCで最初のJobを動かしたい | [QUICKSTART.ja.md](docs/QUICKSTART.ja.md) |
| セットアップ中のエラーを解決したい | [TROUBLESHOOTING.ja.md](docs/TROUBLESHOOTING.ja.md) |
| 2台目以降のNodeを追加したい | [MULTI_NODE.ja.md](docs/MULTI_NODE.ja.md) |
| drain、停止、バックアップ、更新をしたい | [OPERATIONS.ja.md](docs/OPERATIONS.ja.md) |
| Image cacheの容量確認・整理をしたい | [CACHE.ja.md](docs/CACHE.ja.md) |
| 設計・安全性・検証境界を確認したい | [ARCHITECTURE.md](docs/ARCHITECTURE.md) / [SECURITY.md](SECURITY.md) / [VERIFICATION.md](docs/VERIFICATION.md) |

## 仕組み

```text
GitHub Actions
      │  outbound HTTPS
      ▼
 Controller ── 配置判断・状態・GitHub連携・SQLite
      ▲
      │  Nodeからのoutbound mTLS
 ┌────┴────────────┐
 ▼                 ▼
Node A            Node B
libvirt/KVM       libvirt/KVM
新規VM → Job → 削除  新規VM → Job → 削除
```

1. ControllerがGitHubから必要なRunner数を受け取ります。
2. CPU・RAM・ディスク、Pool、予約枠、イメージ、Node状態を照合して配置先を決めます。
3. Node上でGolden Imageからqcow2 overlayを作り、1 Job専用のJIT設定でRunnerを起動します。
4. Job終了後、ホストが停止と所有権を確認してからVM・作業ディスクを削除します。

### 用語

| 用語 | 意味 |
|---|---|
| **Controller** | GitHub連携、配置判断、状態DB、Node認証を担当する管理プロセス |
| **Node / Agent** | VM、libvirt、専用ネットワーク、ローカル資源を管理する実行マシン |
| **Pool** | 1台のVMに割り当てるCPU・RAM・ディスク・最大台数・Runner名の定義 |
| **Golden Image** | Ubuntuと公式Actions Runnerを入れた、未登録の読み取り専用元イメージ |
| **Reservation** | 特定Pool用にNode資源を取り置く管理者定義の枠 |

Workflowから任意のCPU量を指定するのではなく、管理者が事前に定義したPoolを `runs-on` で選びます。

```yaml
jobs:
  build:
    runs-on: home-linux-lite
    steps:
      - run: python3 --version
```

## 1台で使い始める流れ

最初は1台のUbuntu PCをControllerとNodeの兼用にすると、構成を理解しやすくなります。

```text
1. CLIとホスト依存関係を入れる
2. GitHub AppとRunner Groupを用意する
3. Golden Imageを作る
4. setupでPoolと資源上限を決め、設定・CA・Node IDを保存する
5. image importとnetwork applyを明示実行する
6. github check後にController/Agentを起動する
7. 手動Workflowを実行し、VM削除まで確認する
```

コマンド、置換する値、各段階の成功条件は [1台構成セットアップ](docs/QUICKSTART.ja.md) にまとめています。

> [!WARNING]
> `runnerloom setup --apply` は「すべて完了」を意味しません。設定、Controller CA、Node IDを保存しますが、GitHub接続確認、VMネットワーク変更、サービス起動、実Job検証は別の明示操作です。

## どの操作がホストを変更するか

| 操作 | 変更内容 |
|---|---|
| `.deb` / archiveのインストール | CLIバイナリと文書を配置。サービス・ネットワークは起動しない |
| `setup --apply` | 指定したstate directoryへ設定、CA、Node IDを保存 |
| `image build` | 指定先へGolden Imageとmanifestを新規作成 |
| `image import` | Controllerの配布用Image cacheへ検証済みイメージを登録 |
| `cache seed` | 同一filesystemのController/Node cacheを検証済みhard linkで共有 |
| `cache prune --apply` | 所有service停止と再検査後、参照されない古いImage/partialだけを削除 |
| `network apply` | RunnerLoom専用libvirt networkとfirewall規則を作成 |
| `service install --start` | RunnerLoom用systemd unitを生成して起動 |
| GitHub Job | 使い捨てVMと作業ディスクを作成し、完了後に所有確認して削除 |

RunnerLoomはインストールだけで既存ネットワークを書き換えたり、Nodeを勝手に登録したり、サービスを自動起動したりしません。

## 主な機能

- 1台兼用からLAN内の複数Nodeまで同じCLIで管理
- official `actions/scaleset`連携と1 Job専用JIT設定
- SQLite transactionによるCPU・RAM・ディスク会計、予約枠、再実行保護
- Node承認、TLS 1.3、証明書更新、失効確認
- libvirt/KVM、qcow2 overlay、cloud-init、所有権を確認したcleanup
- Canonical署名とRunner digestを検証するGolden Image builder
- 参照・overlayを再検査するdry-run-first cache prune、Range再開download、同一Host hard-link seed
- LAN、ホスト管理面、peer VMへの到達を制限する専用NAT/firewall
- 人間向けCLIと、`--json` / JSON Schemaによる自動化向けインターフェース

## 配布物

GitHub ReleasesにはLinux/amd64 archive、Debian package、build information、`SHA256SUMS`があります。Repositoryが非公開の場合でも、公開化は必須ではありません。ログイン済みブラウザまたは認証済みGitHub CLIで取得できます。

インストール後は次だけ確認し、その後 [Quickstart](docs/QUICKSTART.ja.md) へ進みます。

```bash
runnerloom version --json
runnerloom doctor
runnerloom --help --json
```

## 安全性と対応範囲

RunnerLoomは、ホスト管理者まで敵対する環境や、すべてのhypervisor脆弱性からの保護を保証するものではありません。現在のAgentはlibvirtとfirewallを操作する信頼済み特権プロセスであり、独立監査済みのprivilege-separated helperではありません。

価値のあるRepository secretsを接続する前に、[SECURITY.md](SECURITY.md) と [VERIFICATION.md](docs/VERIFICATION.md) を読み、実ホストでネットワーク隔離、再起動復旧、資源上限、実GitHub Job、Job後の削除を確認してください。

## 開発

```bash
make check
```

変更の出し方は [CONTRIBUTING.md](CONTRIBUTING.md)、テストが証明すること／しないことは [テスト方針](docs/TESTING.ja.md) です。CIの unit/integration、実Ubuntu VM、Golden Image、梱包、脆弱性スキャンは別々の証拠です。

## License

MIT。詳細は [LICENSE](LICENSE) を参照してください。第三者コンポーネントは各ライセンスに従います。
