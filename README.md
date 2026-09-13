# RunnerLoom

Run each GitHub Actions job in a fresh VM on your own Ubuntu machines.

A single Go binary (CLI, Controller, and Agent) polls GitHub’s official scale-set API over **outbound HTTPS**. There is no inbound webhook. Each job gets a new libvirt VM and a one-job JIT runner; the host confirms stop and ownership before disks are deleted.

**Release:** `0.1.0-rc.4` (candidate). **Host:** Ubuntu 24.04 x86_64, CPU VMs, trusted admin. **Jobs:** only GitHub repositories you list that are **private**. GPU, Windows/macOS hosts, controller HA, and public-repo / public-fork jobs are not qualified in this release (hosts other than Ubuntu may be added later).

[Releases](https://github.com/MOVEI144/RunnerLoom/releases) · [Docs index](docs/README.md) · [Security](SECURITY.md) · [Verification](docs/VERIFICATION.md)

---

## これは何か

自宅や社内の Ubuntu を、GitHub Actions の **Job 専用の使い捨て VM** にするソフトです。

```text
Workflow  (runs-on: Poolの名前)
      │
      │  GitHub が Scale Set に「何台欲しいか」を載せる
      ▼
 Controller  ←── 外向き HTTPS（webhook は受け取らない）
      ▲
      │  Node からの外向き mTLS
      ▼
 Node (libvirt/KVM)  新規VM → 1 Job → ホストが停止・削除を確認
```

- Workflow は CPU 数を書きません。管理者が決めた Pool を `runs-on` で選びます。
- GitHub 上で Job が success でも、VM の CPU / RAM / ディスクは **ホストが消したと確認するまで** 返しません。
- CLI を入れただけでは、service は起動せず、ネットワークも変わりません。

```yaml
jobs:
  build:
    runs-on: home-linux-lite
    steps:
      - run: python3 --version
```

## 迷ったらここから

| やりたいこと | 読む場所 |
|---|---|
| 1台で最初の Job まで | 下の「1台で動かす」→ 全文は [QUICKSTART.ja.md](docs/QUICKSTART.ja.md) |
| CLI の入れ方・更新・削除 | [INSTALL.md](docs/INSTALL.md) |
| `github check` などが落ちる | [TROUBLESHOOTING.ja.md](docs/TROUBLESHOOTING.ja.md) |
| 2台目の PC を足す | [MULTI_NODE.ja.md](docs/MULTI_NODE.ja.md) |
| 止める・backup・更新 | [OPERATIONS.ja.md](docs/OPERATIONS.ja.md) |
| 設計・検証の境界 | [ARCHITECTURE.md](docs/ARCHITECTURE.md) · [VERIFICATION.md](docs/VERIFICATION.md) |

## 用語

| 用語 | 意味 |
|---|---|
| **Controller** | GitHub とのやり取り、配置、SQLite、Node 認証 |
| **Node / Agent** | その PC で VM と専用ネットワークを実際に操作する側 |
| **Pool** | VM の大きさ・上限・Workflow が書く `runs-on` 名 |
| **Golden Image** | Ubuntu + 公式 Runner 入りの、まだ登録していない読み取り専用イメージ |
| **Reservation** | 特定 Pool 用に、その Node の枠を先に押さえる設定 |

## GitHub で「何が private か」

言葉が混ざります。Job を流してよいかどうかに関係するのは **1行目だけ** です。

| よく出る「private」 | 意味 | Job を流せるか |
|---|---|---|
| GitHub リポジトリが **private** | その repo の可視性 | **関係する。今の版はこれだけ許可** |
| credential を 0600 のファイルに置く | 鍵の置き方 | 関係しない |
| `172.30.240.0/24` | VM 用の LAN | 関係しない |
| RunnerLoom 本体の GitHub repo が公開 | このソフトの置き場 | **関係しない。本体は公開してよい** |

許可は次が揃っているときだけです。`github check` がこれを API で見ます。

1. Cluster 設定の `github.allowedRepositories`（`owner/name` の一覧）
2. そのそれぞれが GitHub 上で **private**
3. Organization の場合だけ: Runner Group が **Selected repositories**、**public を許可しない**、選択リストが 1. と **完全一致**

`github.url` の形で 3. が変わる点に注意してください。

| `github.url` | 例 | 追加チェック |
|---|---|---|
| Organization | `https://github.com/my-org` | Runner Group の選択リストまで見る |
| リポジトリ1つ | `https://github.com/my-org/app` | `allowedRepositories` はその1つだけ。Group のリスト照合はしない |

Workflow は、許可した **その private リポジトリ** に置き、`runs-on` に Pool の `runnerName` を書きます。ラベルが合っていても、public なら受けません。

非公開でも、その repo の push や依存関係は VM 内でコードが走ります。秘密を渡すなら、そのリポジトリを信用している、という意味です。詳細は [SECURITY.md](SECURITY.md) です。

## 1台で動かす

1台の Ubuntu 24.04 を Controller と Node の兼用にします。値の決め方と画面操作の全文は [QUICKSTART.ja.md](docs/QUICKSTART.ja.md) です。

### 0. 使う値

```bash
export RL_CONTROLLER_STATE="/var/lib/runnerloom/controller"
export RL_NODE_NAME="node-a"
export RL_NODE_STATE="/var/lib/runnerloom/node-a"
export RL_DISK_DIR="/var/lib/libvirt/images/runnerloom-home-node-a"
export RL_CONTROLLER_URL="https://127.0.0.1:8443"
export RL_NETWORK_CIDR="172.30.240.0/24"
export RL_IMAGE="/var/lib/runnerloom/builds/ubuntu-runner.qcow2"
export RL_POOL="linux-lite"
export RL_BINARY="$(command -v runnerloom)"
```

### 1. CLI と KVM

[INSTALL.md](docs/INSTALL.md) で同じ Release の checksum を確認して CLI を入れます。続けて libvirt などを入れ、`doctor` が通ることを見ます。

```bash
runnerloom version --json
sudo apt-get update
sudo apt-get install -y qemu-kvm qemu-utils libvirt-daemon-system \
  libvirt-clients cloud-image-utils libguestfs-tools nftables \
  dnsmasq-base ubuntu-keyring curl gpgv jq
sudo systemctl enable --now libvirtd
runnerloom doctor --strict
```

### 2. GitHub App と Runner Group

Organization なら Settings → Actions → Runner groups で Group を作ります。

- Repository access: Selected repositories
- 使う **private** リポジトリだけを選ぶ
- Public repositories: 許可しない
- 数値の Group ID を控える（`github.runnerGroupID`）

専用 GitHub App を Organization に入れ、Self-hosted runners は Read and write、Metadata は Read-only。Client ID・Installation ID・`.pem` を 0600 で Controller state に置きます。手順のクリック順は [QUICKSTART §2](docs/QUICKSTART.ja.md#2-github側の利用許可を作る) です。この時点ではまだ `github check` しません。

### 3. Golden Image

```bash
sudo install -d -m 0700 "$(dirname "$RL_IMAGE")"
sudo runnerloom image build --out "$RL_IMAGE"
export RL_IMAGE_DIGEST="$(sudo jq -er '.digest' "$RL_IMAGE.manifest.json")"
```

`registered` が `false`、digest が `sha256:` + 64 桁なら成功です。鍵はイメージに入りません。

### 4. 設定を保存する（まだ起動しない）

```bash
sudo runnerloom setup \
  --state "$RL_CONTROLLER_STATE" \
  --role controller-node \
  --node "$RL_NODE_NAME" \
  --node-state "$RL_NODE_STATE" \
  --disk-dir "$RL_DISK_DIR" \
  --advertise "$RL_CONTROLLER_URL" \
  --network-cidr "$RL_NETWORK_CIDR"
```

質問に答えたあと `--apply` します。ここで保存されるのは設定・CA・Node ID だけです。

> [!WARNING]
> `setup --apply` の成功は「動いている」ではありません。GitHub 確認、network、service、実 Job はまだです。

### 5. Image 登録と専用ネットワーク

```bash
sudo runnerloom image import \
  --state "$RL_CONTROLLER_STATE" \
  --file "$RL_IMAGE" \
  --digest "$RL_IMAGE_DIGEST"

sudo runnerloom network plan --config "$RL_NODE_STATE/agent.json"
sudo runnerloom network apply --config "$RL_NODE_STATE/agent.json"
sudo runnerloom network check --config "$RL_NODE_STATE/agent.json"
```

`plan` を読んでから `apply` します。既存の LAN / VPN と CIDR が重なるときは別の `/24` で setup し直します。Node state と VM disk は同じ filesystem にしてください。

### 6. 接続確認して起動する

```bash
sudo runnerloom github check --state "$RL_CONTROLLER_STATE"

sudo runnerloom service install \
  --role controller \
  --state "$RL_CONTROLLER_STATE" \
  --binary "$RL_BINARY" \
  --listen 127.0.0.1:8443 \
  --advertise "$RL_CONTROLLER_URL" \
  --start

sudo runnerloom service install \
  --role agent \
  --state "$RL_NODE_STATE" \
  --config "$RL_NODE_STATE/agent.json" \
  --binary "$RL_BINARY" \
  --start

sudo runnerloom pool explain "$RL_POOL" --state "$RL_CONTROLLER_STATE"
```

`github check` が落ちたら service は起動しません。初回は Image 取得中に `IMAGE_MISSING` が出ることがあります。journal を見て、消えてから `pool explain` を再実行します。

### 7. 手動 Job と削除確認

許可したリポジトリに、`runs-on` が Pool の `runnerName`（例: `home-linux-lite`）の手動 Workflow を置き、Actions から実行します。最初は Repository secrets を使わないでください。例は [QUICKSTART §7](docs/QUICKSTART.ja.md#7-最初のgithub-actions-jobを実行する) です。

```bash
sudo runnerloom vm list --state "$RL_CONTROLLER_STATE"
sudo runnerloom status --state "$RL_CONTROLLER_STATE"
```

GitHub 側が success でも、VM が `Deleted` になり保持資源が 0 になるまで、1台構成は完了しません。

## どの操作がホストを変更するか

| 操作 | 実際に変わるもの |
|---|---|
| CLI の `.deb` / archive | バイナリと文書だけ。service は起動しない |
| `setup --apply` | state へ設定・CA・Node ID |
| `image build` / `image import` | qcow2 と Controller の image cache |
| `network apply` | RunnerLoom 専用の libvirt 網と firewall だけ |
| `service install --start` | systemd unit を作って起動 |
| 実 GitHub Job | その Job の VM と作業ディスク。終わったら所有確認して削除 |

## よくある詰まり

| 症状 | 先に見ること |
|---|---|
| `github check` が失敗 | 許可 repo が GitHub 上で private か。Group が Selected で public オフか。選択リストと設定が同じ集合か。App の権限と 0600 の credential |
| `setup --apply` したのに動かない | 正常。続けて import / network / `github check` / service / 実 Job |
| Job が来ない・`NO_CAPACITY` | `runnerloom pool explain <pool>`。Image・network・資源・Node 許可の理由コードが出る |

切り分けのコマンドは [TROUBLESHOOTING.ja.md](docs/TROUBLESHOOTING.ja.md) です。2台目は [MULTI_NODE.ja.md](docs/MULTI_NODE.ja.md)、止める・backup は [OPERATIONS.ja.md](docs/OPERATIONS.ja.md) です。

## 開発

```bash
make check
```

[CONTRIBUTING.md](CONTRIBUTING.md) · [TESTING.md](docs/TESTING.md) · [VERIFICATION.md](docs/VERIFICATION.md)

## License

MIT。[LICENSE](LICENSE) を参照してください。
