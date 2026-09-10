# 1台のUbuntu PCでRunnerLoomを動かす

このガイドは、1台のUbuntu PCを **ControllerとNodeの兼用** にし、GitHub Actionsの手動Jobを使い捨てVMで実行して、VM削除まで確認する手順です。

[ドキュメント一覧へ戻る](README.md) · [インストール](INSTALL.md) · [トラブル対応](TROUBLESHOOTING.ja.md)

> [!IMPORTANT]
> 最初は非公開Repository、CPU VM、1台構成で確認してください。GPU、Windows/macOSホスト、Controller HA、公開fork由来のJobはこの版の対象外です。

## このガイドの完了条件

最後に次の状態を確認します。

- `runnerloom doctor --strict` が成功する
- `runnerloom github check` が成功する
- `runnerloom network check` が成功する
- ControllerとAgentのsystemd serviceがactiveになる
- `runnerloom pool explain` で対象Nodeを配置候補として確認できる
- GitHub Actionsの手動Jobが成功する
- Job後にVMが`Deleted`となり、保持資源が解放される

`setup --apply` の成功だけでは、ここまで完了したことにはなりません。

## セットアップの流れ

| 段階 | 作るもの | ホスト変更 |
|---|---|---|
| 1 | KVM/libvirt実行環境 | packageとlibvirtdを追加 |
| 2 | GitHub App、Runner Group、credential file | GitHub側とprivate fileを作成 |
| 3 | Golden Image | qcow2とmanifestを新規作成 |
| 4 | Cluster設定、CA、同居Node ID | state directoryへ保存 |
| 5 | Image cache、専用VM network | 明示したcache/networkだけ変更 |
| 6 | Controller/Agent service | RunnerLoom所有のsystemd unitを追加 |
| 7 | 最初のJob | VMを作成し、完了後に削除 |

## 0. このガイドで使う値を決める

以下は1台構成の例です。別の値を使う場合は、以降のコマンドでも同じ値へ置き換えてください。

| 項目 | このガイドの例 | 意味 |
|---|---|---|
| Cluster名 | `home` | RunnerLoom環境全体の名前 |
| Node名 | `node-a` | このUbuntu PCの名前 |
| Pool名 | `linux-lite` | 管理者が定義するVMサイズ |
| Runner名 | `home-linux-lite` | Workflowの`runs-on`に書く名前 |
| Controller state | `/var/lib/runnerloom/controller` | Controller DB、CA、GitHub credential |
| Node state | `/var/lib/runnerloom/node-a` | Node ID、image cache、network seal、log |
| VM disk | `/var/lib/libvirt/images/runnerloom-home-node-a` | qcow2 overlayと作業disk |
| Controller URL | `https://127.0.0.1:8443` | 同じPC上のNodeから見えるController |
| VM network | `172.30.240.0/24` | RunnerLoom専用のprivate `/24` |

以降のコマンドを短くするため、現在のshellに変数を設定します。

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

# Image作成後にterminalを開き直した場合、manifestからdigestも復元する
if sudo test -f "$RL_IMAGE.manifest.json"; then
  export RL_IMAGE_DIGEST="$(
    sudo python3 -c '
import json, re, sys
value = json.load(open(sys.argv[1], encoding="utf-8"))["digest"]
if not re.fullmatch(r"sha256:[0-9a-f]{64}", value):
    raise SystemExit("invalid image digest in manifest")
print(value)
' "$RL_IMAGE.manifest.json"
  )"
fi
```

新しいterminalを開いた場合は、このblockをもう一度実行してください。Golden Image作成後なら`RL_IMAGE_DIGEST`も自動的に復元されます。

## 1. Hostを準備する

### 1-1. CLIを確認する

[INSTALL.md](INSTALL.md) の手順でRunnerLoomをインストールした後、意図したbinaryが選ばれているか確認します。

```bash
command -v runnerloom
runnerloom version --json
```

### 1-2. KVM/libvirtとImage builderを入れる

```bash
sudo apt-get update
sudo apt-get install -y \
  qemu-kvm qemu-utils \
  libvirt-daemon-system libvirt-clients \
  cloud-image-utils libguestfs-tools \
  nftables dnsmasq-base \
  ubuntu-keyring curl gpgv jq

sudo systemctl enable --now libvirtd
sudo virsh -c qemu:///system version
runnerloom doctor --strict
```

このapt操作はホストへpackageとserviceを追加します。RunnerLoomが裏で自動実行するものではありません。

### 成功条件

- `/dev/kvm` が存在する
- `sudo virsh -c qemu:///system version` がversionを表示する
- `runnerloom doctor --strict` が終了コード0になる

`/dev/kvm` がない場合は、BIOS/UEFIのIntel VT-x / AMD-V、仮想化の入れ子、Ubuntu側のKVM moduleを先に確認してください。

## 2. GitHub側の利用許可を作る

RunnerLoomは、普段のGitHub login tokenを抽出しません。専用GitHub Appを推奨します。

### 2-1. Runner Groupを作る

Organizationの画面で次の順に開きます。

```text
Organization
  → Settings
  → Actions
  → Runner groups
  → New runner group
```

設定は次のようにします。

| 設定 | 推奨値 |
|---|---|
| Repository access | `Selected repositories` |
| Selected repositories | RunnerLoomを使う非公開Repositoryだけ |
| Public repositories | 許可しない |
| Workflow access | 必要なら実行を許可するWorkflowへ限定 |

作成後、Runner Groupの **数値ID** を控えます。IDが画面に見えない場合は、Organization名を置換して一覧を取得できます。

```bash
org="OWNER"
gh api "/orgs/$org/actions/runner-groups" \
  --jq '.runner_groups[] | [.id, .name, .visibility] | @tsv'
```

使用するGroup名と同じ行の先頭の数値が`runnerGroupID`です。

GitHub公式: [Managing access to self-hosted runners using groups](https://docs.github.com/en/actions/how-tos/manage-runners/self-hosted-runners/manage-access)

### 2-2. 専用GitHub Appを作る

OrganizationにインストールするGitHub Appを作り、少なくとも次を設定します。

| Permission | Level | 用途 |
|---|---|---|
| Organization: Self-hosted runners | Read and write | Runner Group、JIT runner、登録解除など |
| Repository: Metadata | Read-only | 許可Repositoryの照合 |

必要以上のRepository権限を付けないでください。GitHubのAPI要件が変わった場合は、各endpointの公式permission表を優先します。

- Appを対象Organizationへインストールする
- Client IDを控える
- Installation IDを控える
- private keyを生成して`.pem`を取得する

GitHub公式: [Choosing permissions for a GitHub App](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/choosing-permissions-for-a-github-app) / [Self-hosted runner REST API](https://docs.github.com/en/rest/actions/self-hosted-runners)

### 2-3. credentialをprivate fileとして保存する

`YOUR_APP_PRIVATE_KEY.pem`、`YOUR_CLIENT_ID`、`YOUR_INSTALLATION_ID`を実値へ置換します。

```bash
sudo install -d -m 0700 "$RL_CONTROLLER_STATE"
sudo install -m 0600 YOUR_APP_PRIVATE_KEY.pem \
  "$RL_CONTROLLER_STATE/github-app.pem"

sudo install -m 0600 /dev/null \
  "$RL_CONTROLLER_STATE/github-credentials.json"
sudoedit "$RL_CONTROLLER_STATE/github-credentials.json"
```

`github-credentials.json`:

```json
{
  "clientID": "YOUR_CLIENT_ID",
  "installationID": 123456,
  "privateKeyFile": "/var/lib/runnerloom/controller/github-app.pem"
}
```

確認します。

```bash
sudo jq -e '
  (.clientID | type == "string" and length > 0) and
  (.installationID | type == "number" and . > 0) and
  (.privateKeyFile | type == "string" and startswith("/"))
' "$RL_CONTROLLER_STATE/github-credentials.json" >/dev/null
sudo test -r "$RL_CONTROLLER_STATE/github-app.pem"
echo 'GitHub App credential files: readable'
```

この時点ではGitHub API接続をまだ確認していません。`github check`はCluster設定保存後に実行します。

## 3. Golden Imageを作る

```bash
sudo install -d -m 0700 "$(dirname "$RL_IMAGE")"
sudo runnerloom image build --out "$RL_IMAGE"
```

この処理は、Canonical署名を確認したUbuntu cloud imageと、SHA-256を確認した公式GitHub Actions Runnerから、未登録のqcow2を作ります。App private keyやController keyはImageへ入れません。

生成物を確認します。

```bash
sudo test -f "$RL_IMAGE"
sudo test -f "$RL_IMAGE.manifest.json"
sudo jq '{digest, runnerVersion, os, architecture, registered}' \
  "$RL_IMAGE.manifest.json"

export RL_IMAGE_DIGEST="$(sudo jq -er .digest "$RL_IMAGE.manifest.json")"
printf '%s\n' "$RL_IMAGE_DIGEST"
```

### 成功条件

- qcow2と`.manifest.json`の両方が存在する
- `digest` が `sha256:` と64桁の16進数になる
- `registered` が `false` になる
- `runnerVersion` が実際に解決された版を表示する

`RL_IMAGE_DIGEST`は以降の設定とimportで同じ値を使います。terminalを開き直した場合は、手順0の変数blockを再実行してください。

## 4. Cluster設定と同居Node IDを作る

人が最初に構築するときは、対話式setupを推奨します。JSONによる無人setupは後述します。

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

質問には次を入力します。

| 質問 | 入力するもの |
|---|---|
| 環境の名前 | `home`などのCluster名 |
| GitHub OrganizationのURL | `https://github.com/OWNER` |
| 許可Repository | `OWNER/REPO`。複数ならカンマ区切り |
| Runner Group ID | 手順2で控えた数値 |
| GitHub認証設定JSON | `$RL_CONTROLLER_STATE/github-credentials.json`の実パス |
| Golden Image digest | `$RL_IMAGE_DIGEST`の値 |
| Node名 | `node-a` |
| 実行専用か | 他用途と兼用なら`no`、実行専用なら`yes` |
| CPU/RAM/disk上限 | Host OSと管理処理の余裕を残した合計上限 |

最後に内容を確認し、保存する場合だけ `yes` を入力します。

> [!CAUTION]
> `budget`は「このPCから全VMへ提供する合計上限」です。物理資源と同じ値を入れず、Host OS、libvirt、Controller/Agent、page cache用の余裕を残してください。RAMはMiB、diskはGiBです。

### setupが行うこと

- Cluster設定をController DBへ保存
- Controller CAを作成または再利用
- 同居Nodeのkey、CSR、certificate、`agent.json`を保存

### setupが行わないこと

- GitHub API接続確認
- Golden Image import
- libvirt networkやfirewallの変更
- systemd serviceのinstall/start
- VM起動や実Job検証

### 成功条件

出力で次を確認します。

```text
configured: true
githubReady: false       ← この段階では正常
vmReady: false           ← この段階では正常
networkChanged: false    ← setupが勝手に変更していない
serviceInstalled: false  ← setupが勝手に起動していない
nodeConfig: /var/lib/runnerloom/node-a/agent.json
```

```bash
sudo test -f "$RL_NODE_STATE/agent.json"
sudo runnerloom status --state "$RL_CONTROLLER_STATE"
```

## 5. Imageを登録し、専用VM networkを作る

### 5-1. Controller配布用cacheへImageをimportする

`RL_IMAGE_DIGEST`が空でないことを先に確認します。

```bash
test -n "${RL_IMAGE_DIGEST:-}"

sudo runnerloom image import \
  --state "$RL_CONTROLLER_STATE" \
  --file "$RL_IMAGE" \
  --digest "$RL_IMAGE_DIGEST" \
  --cache-gib 100
```

`verified: true` と、`$RL_CONTROLLER_STATE/images/...qcow2`へのpathを確認します。

### 5-2. network変更をplanしてからapplyする

```bash
sudo runnerloom network plan \
  --config "$RL_NODE_STATE/agent.json"

sudo runnerloom network apply \
  --config "$RL_NODE_STATE/agent.json"

sudo runnerloom network check \
  --config "$RL_NODE_STATE/agent.json"
```

`plan`を先に読み、対象がRunnerLoom専用networkと規則だけであることを確認してから`apply`します。標準CIDRが既存LAN、VPN、container networkと重なる場合は、別のprivate IPv4 `/24`でsetupをやり直してください。

### 5-3. Image cacheとVM diskのfilesystemを確認する

Nodeは読み取り専用base imageをhard linkでVM storageへ公開するため、Node state（その下にimage cacheが作られます）とVM disk directoryは同じfilesystem上に必要です。

```bash
sudo mkdir -p "$RL_DISK_DIR"
findmnt -T "$RL_NODE_STATE"
findmnt -T "$RL_DISK_DIR"
```

2つの`TARGET`が同じfilesystemを示すことを確認します。

## 6. GitHub接続を確認し、serviceを起動する

### 6-1. GitHub AppとRunner Groupを検証する

```bash
sudo runnerloom github check \
  --state "$RL_CONTROLLER_STATE"
```

`accessVerified: true` が成功条件です。失敗した場合はserviceを起動せず、[トラブル対応](TROUBLESHOOTING.ja.md#5-github-checkが失敗する) のGitHub欄を確認してください。

### 6-2. Controller serviceを入れる

```bash
sudo runnerloom service install \
  --role controller \
  --state "$RL_CONTROLLER_STATE" \
  --binary "$RL_BINARY" \
  --listen 127.0.0.1:8443 \
  --advertise "$RL_CONTROLLER_URL" \
  --start
```

この操作は専用の非rootユーザー`runnerloom-controller`を作り、Controller state内のprivate fileをそのユーザーが読めるよう所有者を調整します。

### 6-3. Agent serviceを入れる

```bash
sudo runnerloom service install \
  --role agent \
  --state "$RL_NODE_STATE" \
  --config "$RL_NODE_STATE/agent.json" \
  --binary "$RL_BINARY" \
  --start
```

Agentはlibvirtとfirewallを操作する信頼済みhost processとして動きます。Job自体がhost rootとして動くという意味ではありません。

### 6-4. 状態を確認する

```bash
systemctl --no-pager --full status runnerloom-controller.service
systemctl --no-pager --full status "runnerloom-agent-$RL_NODE_NAME.service"

sudo runnerloom status --state "$RL_CONTROLLER_STATE"
sudo runnerloom pool explain "$RL_POOL" --state "$RL_CONTROLLER_STATE"
```

初回はAgentがControllerからImageを取得・検証するまで、`IMAGE_MISSING`が表示されることがあります。journalを確認し、取得完了後に`pool explain`を再実行します。

```bash
sudo journalctl -u runnerloom-controller.service -n 100 --no-pager
sudo journalctl -u "runnerloom-agent-$RL_NODE_NAME.service" -n 100 --no-pager
```

### 成功条件

- 2つのserviceが`active (running)`
- Nodeの報告が新しい
- `NODE_NOT_READY`、`NETWORK_NOT_VERIFIED`、`IMAGE_MISSING`が解消する
- PoolのVMサイズがNodeのavailable資源へ収まる

## 7. 最初のGitHub Actions Jobを実行する

許可したRepositoryへ `.github/workflows/runnerloom-smoke.yml` を追加します。

```yaml
name: RunnerLoom smoke test

on:
  workflow_dispatch:

permissions:
  contents: read

jobs:
  smoke:
    runs-on: home-linux-lite
    timeout-minutes: 10
    steps:
      - name: Confirm disposable VM
        shell: bash
        run: |
          set -euo pipefail
          uname -a
          python3 --version
          git --version
          printf 'runnerloom smoke\n' > /tmp/runnerloom-smoke.txt
          sha256sum /tmp/runnerloom-smoke.txt
          echo 'Executed inside a disposable RunnerLoom VM'
```

`runs-on`には、setupで作成したPoolの`runnerName`を指定します。分からない場合は確認します。

```bash
sudo runnerloom pool list --state "$RL_CONTROLLER_STATE"
```

Workflowをdefault branchへ保存し、GitHub Actions画面から手動実行します。最初はRepository secretsを使わないJobにしてください。

### Job後の確認

```bash
sudo runnerloom vm list --state "$RL_CONTROLLER_STATE"
sudo runnerloom status --state "$RL_CONTROLLER_STATE"
sudo journalctl -u "runnerloom-agent-$RL_NODE_NAME.service" -n 150 --no-pager
```

次を確認します。

- GitHub Actions側がsuccess
- logに`Executed inside a disposable RunnerLoom VM`がある
- 対象VMが最終的に`Deleted`
- CPU、RAM、diskの保持量が解放されている
- 次のJobを投入しても古いVMやrunner registrationを再利用しない

ここまで確認できれば、1台構成の初回セットアップは完了です。

## 8. 最終チェックリスト

```bash
runnerloom version --json
runnerloom doctor --strict
sudo runnerloom github check --state "$RL_CONTROLLER_STATE"
sudo runnerloom network check --config "$RL_NODE_STATE/agent.json"
sudo runnerloom status --state "$RL_CONTROLLER_STATE"
sudo runnerloom pool explain "$RL_POOL" --state "$RL_CONTROLLER_STATE"
systemctl is-active runnerloom-controller.service
systemctl is-active "runnerloom-agent-$RL_NODE_NAME.service"
```

| 確認 | 合格状態 |
|---|---|
| Binary | 意図したReleaseとsource commit |
| Host | `doctor --strict`成功 |
| GitHub | `accessVerified: true` |
| Network | `verified: true` |
| Services | Controller / Agentとも`active` |
| Placement | 対象Nodeに拒否理由がない |
| Job | 手動smoke test成功 |
| Cleanup | VMが`Deleted`、保持資源0 |

## JSONで無人セットアップする場合

対話式で構成を理解した後は、JSONをversion control外の安全な場所で管理できます。

```bash
runnerloom config sample > cluster.json
runnerloom config schema > cluster.schema.json
editor cluster.json
runnerloom config validate --file cluster.json --json
```

最低限、次を実値へ変更します。

| JSON field | 変更内容 |
|---|---|
| `github.url` | Organization URL |
| `github.runnerGroupID` | 手順2の数値ID |
| `github.credentialFile` | private credential JSONの絶対path |
| `github.allowedRepositories` | Runner Groupで許可したRepository |
| `images[].digest` | `$RL_IMAGE_DIGEST` |
| `nodes[].budget` | このPCから全VMへ提供する合計上限 |
| `nodes[].localCeiling` | Node管理者が許可する絶対上限 |
| `pools[]` | 1 VMのサイズ、最大台数、`runnerName` |

最初はPoolを1つ、`maxRunners: 1`、`reservations: []`にすると確認しやすくなります。

```bash
sudo runnerloom setup \
  --state "$RL_CONTROLLER_STATE" \
  --file "$PWD/cluster.json" \
  --role controller-node \
  --node "$RL_NODE_NAME" \
  --node-state "$RL_NODE_STATE" \
  --disk-dir "$RL_DISK_DIR" \
  --advertise "$RL_CONTROLLER_URL" \
  --network-cidr "$RL_NETWORK_CIDR" \
  --apply --non-interactive --json
```

JSON setupでも、その後の`image import`、`network plan/apply/check`、`github check`、`service install`、実Job確認は省略できません。

## 次に読む

- 日常のdrain、Job停止、backup、maintenance、更新: [OPERATIONS.ja.md](OPERATIONS.ja.md)
- 2台目以降のNode追加、LAN URL、招待、承認: [MULTI_NODE.ja.md](MULTI_NODE.ja.md)
- setupやJobが進まない場合: [TROUBLESHOOTING.ja.md](TROUBLESHOOTING.ja.md)
- 実装済みと未検証の境界: [VERIFICATION.md](VERIFICATION.md)
- 権限とthreat model: [SECURITY.md](../SECURITY.md)
