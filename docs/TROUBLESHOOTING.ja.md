# RunnerLoomトラブルシューティング

このページは、セットアップやJobが進まないときに「どの層で止まっているか」を切り分けるための手順です。

[ドキュメント一覧へ戻る](README.md) · [1台構成](QUICKSTART.ja.md) · [運用](OPERATIONS.ja.md)

## 最初に実行する確認

例ではController stateを`/var/lib/runnerloom/controller`、Nodeを`node-a`とします。

```bash
export RL_CONTROLLER_STATE="/var/lib/runnerloom/controller"
export RL_NODE_NAME="node-a"
export RL_NODE_STATE="/var/lib/runnerloom/node-a"
export RL_NODE_CONFIG="$RL_NODE_STATE/agent.json"
```

次を上から順に実行します。

```bash
runnerloom version --json
runnerloom doctor
sudo runnerloom status --state "$RL_CONTROLLER_STATE" --json
sudo runnerloom github check --state "$RL_CONTROLLER_STATE"
sudo runnerloom network check --config "$RL_NODE_CONFIG"
sudo runnerloom pool explain linux-lite --state "$RL_CONTROLLER_STATE" --json
```

| 失敗した場所 | 主に確認するもの |
|---|---|
| `version` | binary path、install方式、Release |
| `doctor` | OS、KVM、libvirt、qemu、nftables、dnsmasq |
| `status` | setup、Controller DB、Node heartbeat、VM state |
| `github check` | GitHub App、Runner Group、Repository許可 |
| `network check` | Node設定、専用network、firewall、CIDR衝突 |
| `pool explain` | Pool、Node許可、Image、資源、予約枠 |

## 1. 実行しているbinaryが違う

### 症状

- 更新したのにversionが古い
- 文書にあるflagが見つからない
- `.deb`を更新したのに挙動が変わらない

### 確認

```bash
type -a runnerloom
command -v runnerloom
runnerloom version --json
```

`/usr/local/bin/runnerloom`と`/usr/bin/runnerloom`が両方存在すると、通常は`/usr/local/bin`が優先されます。Archive版と`.deb`版の片方だけを残してください。

## 2. `doctor --strict`が失敗する

### KVMがない

```bash
ls -l /dev/kvm
lsmod | grep -E '^kvm'
lscpu | grep -i virtualization
```

- BIOS/UEFIでIntel VT-x / AMD-Vを有効にする
- VM上のHostならnested virtualizationを確認する
- Ubuntu kernel moduleと権限を確認する

### libvirtに接続できない

```bash
systemctl status libvirtd --no-pager
sudo systemctl enable --now libvirtd
sudo virsh -c qemu:///system version
```

### commandが不足している

Quickstartのhost packageを再確認します。

```bash
sudo apt-get install -y \
  qemu-kvm qemu-utils libvirt-daemon-system libvirt-clients \
  cloud-image-utils nftables dnsmasq-base ubuntu-keyring curl gpgv jq
```

Golden Imageを同じHostで作る場合は`libguestfs-tools`も必要です。

## 3. private path / permissionで失敗する

### 症状

- `symlink in private path`
- `unsafe ... path`
- `permission denied`
- Controller serviceだけcredentialを読めない

RunnerLoomは、秘密状態のpathにsymlink、world-writable component、不正なfile typeがある場合に拒否します。

```bash
namei -l /var/lib/runnerloom/controller
sudo find /var/lib/runnerloom/controller -maxdepth 2 -printf '%M %u:%g %p\n'
sudo find /var/lib/runnerloom/node-a -maxdepth 2 -printf '%M %u:%g %p\n'
```

private keyやcredential fileを`chmod 644`で回避しないでください。Controller serviceのinstallは、Controller state treeを専用`runnerloom-controller`ユーザーへ安全にchownします。再生成前に同じstateを使うControllerを止めます。次のURLは1台構成の例なので、複数Node構成では実際のlisten/advertise値へ置き換えてください。

```bash
sudo systemctl stop runnerloom-controller.service
sudo runnerloom service install \
  --role controller \
  --state /var/lib/runnerloom/controller \
  --binary "$(command -v runnerloom)" \
  --listen 127.0.0.1:8443 \
  --advertise https://127.0.0.1:8443 \
  --start
```

同じstate directoryを手動Controller processが使っている場合も停止してください。permission変更でprocess lockを回避しないでください。

## 4. `setup`は成功したが何も動かない

これは正常な場合があります。`setup --apply`は設定、CA、Node IDを保存しますが、次は別操作です。

```text
image import
network plan / apply / check
github check
service install --start
実GitHub Job
```

setup出力の次は未確認を示します。

```text
githubReady: false
vmReady: false
networkChanged: false
serviceInstalled: false
```

[Quickstart](QUICKSTART.ja.md#5-imageを登録し専用vm-networkを作る) の手順5以降を続けてください。

## 5. `github check`が失敗する

credential JSONの全文を表示しないでください。Client ID、Installation ID、参照先pathは秘密鍵本文ではありませんが、共有用診断には不要な識別情報です。次の確認は値を表示せず、構造・権限・参照先の可読性だけを検査します。

```bash
credential_file="$RL_CONTROLLER_STATE/github-credentials.json"

sudo jq -e '
  ((keys | sort) == ["tokenFile"] and
    (.tokenFile | type == "string" and startswith("/"))) or
  ((keys | sort) == ["clientID", "installationID", "privateKeyFile"] and
    (.clientID | type == "string" and length > 0) and
    (.installationID | type == "number" and . > 0) and
    (.privateKeyFile | type == "string" and startswith("/")))
' "$credential_file" >/dev/null &&
  echo 'credential JSON structure: OK'

sudo stat -c 'credential JSON mode=%a owner=%U:%G' "$credential_file"
sudo sh -eu -c '
  material="$(jq -r ".privateKeyFile // .tokenFile" "$1")"
  test -n "$material"
  test -r "$material"
  stat -c "referenced material mode=%a owner=%U:%G" "$material"
' sh "$credential_file"
```

この出力はIDやpathを表示しません。それでも外部共有前にOrganization名、Repository名、内部addressなどを確認してください。

Cluster設定とGitHub側で、次を一致させます。

| 項目 | 確認先 |
|---|---|
| Organization URL | `github.url` |
| Runner Group数値ID | `github.runnerGroupID` |
| 許可Repository | `github.allowedRepositories`とRunner GroupのSelected repositories |
| App installation | 対象Organizationへinstall済みか |
| App Client ID | credential JSON |
| Installation ID | credential JSON |
| Private key path | credential JSONから読める0600 file |
| App permission | Organization Self-hosted runners: read/write |
| Repository metadata | Metadata: read |

Runner Groupでpublic Repositoryを許可せず、最初は対象private Repositoryだけを選びます。

### 既存Scale Setを勝手に採用しない

同じRunner名がGitHub側にあり、RunnerLoomの所有記録がない場合は自動採用しません。自分が作成したScale Setだと別経路で確認した場合だけ、表示されたIDを指定します。

```bash
sudo runnerloom github adopt \
  --state "$RL_CONTROLLER_STATE" \
  --pool linux-lite \
  --id SCALE_SET_ID
```

不明なScale Setは削除・adoptせず、別の`runnerName`を使ってください。

## 6. `network plan/apply/check`が失敗する

### CIDRが既存routeと重なる

```bash
ip -4 route
ip -4 addr
sudo virsh -c qemu:///system net-list --all
```

Node設定の`networkCIDR`を、LAN、VPN、Docker/Podman、別のRunnerLoom networkと重ならないprivate IPv4 `/24`へ変更します。変更後はplanを読み直してからapplyします。

```bash
sudo runnerloom network plan --config "$RL_NODE_CONFIG"
sudo runnerloom network apply --config "$RL_NODE_CONFIG"
sudo runnerloom network check --config "$RL_NODE_CONFIG"
```

### firewall reload後に失敗する

RunnerLoom所有のnetwork sealがある状態でAgent serviceを起動すると、明示承認済みnetworkを復元します。先に`network apply`が成功している必要があります。

```bash
sudo test -f "$RL_NODE_STATE/network/network-seal.json"
sudo systemctl restart "runnerloom-agent-$RL_NODE_NAME.service"
```

他のfirewall managerが規則を書き換えるHostでは、適用順序と所有境界を確認してください。

## 7. `pool explain`の理由コード

| 理由 | 意味 | 主な対処 |
|---|---|---|
| `POOL_DISABLED` | Poolが無効 | `enabled`と適用済みconfigを確認 |
| `NODE_NOT_ALLOWED` | NodeがPoolを許可していない | `nodes[].allowedPools`とselectorを確認 |
| `POOL_LIMIT` | Poolの最大同時数へ到達 | Job完了を確認。必要なら安全な資源計算後に`maxRunners`変更 |
| `NODE_NOT_READY` | heartbeatなし、古い、drain中、readyでない | Agent service、Controller URL、時刻、drain状態を確認 |
| `NETWORK_NOT_VERIFIED` | Nodeが隔離networkを確認できていない | `network check`、Agent journalを確認 |
| `LOCAL_CEILING` | Cluster budgetがNode管理者の承認上限を超える | Cluster側budgetかNode側ceilingを見直す |
| `IMAGE_MISSING` | Node cacheにdigest一致Imageがない | Controller import、Agent download、disk容量、digestを確認 |
| `INSUFFICIENT_BUDGET` | 他VM・予約枠を引くとPoolサイズが入らない | held資源、reservation、Poolサイズを確認 |
| `INSUFFICIENT_PHYSICAL_DISK` | 実filesystemの空きと安全余裕が不足 | VM/cache容量を確保。記録だけを削除して誤魔化さない |
| `RESERVATION_INCONSISTENT` | 予約定義と実行記録が矛盾 | config、対象Pool、実行中VMを確認してから変更 |

`NO_CAPACITY`は、候補Nodeがないか、先頭候補に上記の拒否理由が残っている状態です。`pool explain`の詳細を先に直します。

## 8. `IMAGE_MISSING`が消えない

Controller cache:

```bash
sudo find "$RL_CONTROLLER_STATE/images" -maxdepth 1 -type f -ls
```

Node cache:

```bash
sudo find "$RL_NODE_STATE/images" -maxdepth 1 -type f -ls
sudo journalctl -u "runnerloom-agent-$RL_NODE_NAME.service" -n 200 --no-pager
```

確認項目:

1. Cluster設定の`images[].digest`
2. `image import --digest`に指定した値
3. manifestの`digest`
4. Node download後のfile名
5. cache上限とphysical disk空き
6. Controller/Agent間のmTLS接続

digestを架空の値へ変更したり、検証を飛ばしたfileを手動renameしたりしないでください。

容量、partial、保護理由、base hard linkをまとめて確認できます。

```bash
sudo runnerloom cache status --state "$RL_CONTROLLER_STATE" --cache-gib 100
sudo runnerloom cache status --config "$RL_NODE_CONFIG" --verify
```

`safeToPrune: false`なら警告を先に解消します。手動`rm`ではなく、[cache管理手順](CACHE.ja.md)のdry-run-first pruneを使用してください。

## 9. Image cacheとVM diskが別filesystem

### 症状

- hard link作成に失敗する
- `image cache and VM storage must share a filesystem`

Node image cacheはNode stateの下に作られるため、存在済みのNode stateとVM disk directoryのmountを比較します。

```bash
findmnt -T "$RL_NODE_STATE"
findmnt -T /var/lib/libvirt/images/runnerloom-home-node-a
```

`TARGET`が同じfilesystemになるよう、Node stateまたは専用VM disk directoryを配置します。symlinkで繋ぐ方法はprivate path検証と所有境界を壊すため使用しません。

## 10. Controller / Agent serviceが起動しない

```bash
systemctl --no-pager --full status runnerloom-controller.service
systemctl --no-pager --full status "runnerloom-agent-$RL_NODE_NAME.service"
sudo journalctl -u runnerloom-controller.service -b --no-pager
sudo journalctl -u "runnerloom-agent-$RL_NODE_NAME.service" -b --no-pager
```

代表例:

| Error | 確認 |
|---|---|
| `ALREADY_RUNNING` | 同じstateを使う手動processまたは旧serviceを停止 |
| `CONTROLLER_RUNNING` | maintenance前にController serviceを停止 |
| `NOT_CONFIGURED` | `setup --apply`または`config apply`が完了しているか |
| credential read error | Controller state内のowner/mode、private key path |
| network seal missing | Agent install前に`network apply` |
| controller URL error | `agent.json`、invite URL、`--advertise`を一致 |
| certificate error | CA fingerprint、hostname/IP、Node identity、時刻 |

手動の`controller run`とsystemd serviceを同じstateで同時に起動しないでください。

## 11. Node reportが古い / `STALE_REPORT`

```bash
sudo runnerloom node list --state "$RL_CONTROLLER_STATE" --json
sudo journalctl -u "runnerloom-agent-$RL_NODE_NAME.service" -n 200 --no-pager
```

- NodeとControllerのsystem clockを確認
- 同じNode identityを複数PCへコピーしていないか確認
- 古いAgent processが残っていないか確認
- sequence fileを削除して回避しない

RunnerLoomは古いsequenceを新しい空き報告として受け入れません。Node identityの複製がある場合は、一方を停止して明示的に再登録します。

## 12. Job終了後もVMが残る

Controller:

```bash
sudo runnerloom vm list --state "$RL_CONTROLLER_STATE" --json
sudo runnerloom status --state "$RL_CONTROLLER_STATE" --json
```

Node:

```bash
sudo virsh -c qemu:///system list --all
sudo journalctl -u "runnerloom-agent-$RL_NODE_NAME.service" -n 250 --no-pager
```

RunnerLoomは`Unknown`を`Deleted`として扱いません。まずdomain stateと所有metadataを確認します。停止済みのRunnerLoom所有VMだけを手動回収する場合は、Agentを停止して`vm recover`を使います。

```bash
sudo systemctl stop "runnerloom-agent-$RL_NODE_NAME.service"
sudo runnerloom vm recover VM_ID --config "$RL_NODE_CONFIG"
sudo systemctl start "runnerloom-agent-$RL_NODE_NAME.service"
```

`virsh undefine`やdiskの手動削除を先に行うと、ControllerとNodeの所有証拠が食い違います。

## 13. GitHub Jobがqueueのまま

次を順に確認します。

1. Workflowの`runs-on`がPoolの`runnerName`と完全一致
2. Controller serviceがactive
3. `github check`が成功
4. Poolがenabled
5. Nodeがreadyかつdrainされていない
6. `pool explain`に拒否理由がない
7. Runner Groupが対象Repository/Workflowを許可
8. GitHub側に同名の未所有Scale Setがない

```bash
sudo runnerloom pool list --state "$RL_CONTROLLER_STATE"
sudo runnerloom pool explain linux-lite --state "$RL_CONTROLLER_STATE"
```

## 調査情報を保存する

診断fileは収集中からprivateにします。次のblockはsubshell内で`umask 077`を設定し、directoryを0700で作成します。リダイレクトで作られるfileも最初からowner以外には読めません。

```bash
(
  set -euo pipefail
  umask 077
  install -d -m 0700 runnerloom-diagnostics

  runnerloom version --json > runnerloom-diagnostics/version.json
  runnerloom doctor > runnerloom-diagnostics/doctor.txt
  sudo runnerloom status --state "$RL_CONTROLLER_STATE" --json \
    > runnerloom-diagnostics/status.json
  sudo runnerloom node list --state "$RL_CONTROLLER_STATE" --json \
    > runnerloom-diagnostics/nodes.json
  sudo runnerloom pool explain linux-lite --state "$RL_CONTROLLER_STATE" --json \
    > runnerloom-diagnostics/pool-explain.json
  sudo runnerloom vm list --state "$RL_CONTROLLER_STATE" --json \
    > runnerloom-diagnostics/vms.json
  sudo journalctl -u runnerloom-controller.service --since -1h --no-pager \
    > runnerloom-diagnostics/controller.log
  sudo journalctl -u "runnerloom-agent-$RL_NODE_NAME.service" --since -1h --no-pager \
    > runnerloom-diagnostics/agent.log
)
```

共有前に次を必ず除外します。

- GitHub App private key、PAT、installation token
- Client ID、Installation ID、credentialやprivate keyのpath
- invitation secret
- JIT configuration
- Node private key
- Controller CA private key、master key
- private Repository名や内部addressなど、公開不要な環境情報

Security issueは公開Issueへ詳細を書かず、[SECURITY.md](../SECURITY.md) の手順を使ってください。
