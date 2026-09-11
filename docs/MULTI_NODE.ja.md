# 2台目以降のNodeを追加する

このページは、1台構成で実JobとVM cleanupまで確認済みのRunnerLoomへ、LAN内のUbuntu Nodeを追加する手順です。

[ドキュメント一覧へ戻る](README.md) · [1台構成](QUICKSTART.ja.md) · [トラブル対応](TROUBLESHOOTING.ja.md)

> [!IMPORTANT]
> Node追加には、Controllerから見たCluster設定と、追加Node自身の`agent.json`の両方が必要です。招待と証明書だけでは、Poolへ配置できません。

## 完了条件

- 追加NodeがCluster設定の`nodes[]`に存在する
- NodeからControllerのHTTPS URLへ到達できる
- 招待、CSR fingerprint確認、明示承認が完了する
- `network check`が成功する
- Agent serviceがactiveになる
- `pool explain`で追加Nodeの拒否理由が解消する
- 実Jobを追加Nodeへ配置でき、Job後にVMを削除できる

## 全体の流れ

```text
1. ControllerをLANから到達できるURLへ変更
2. Cluster設定へnode-bを追加
3. Controllerで期限付き招待を作成
4. node-bで参加申請
5. ControllerでCSR fingerprintを確認して承認
6. node-bで同じ参加処理を再実行して証明書を取得
7. node-bでnetwork applyとAgent service起動
8. pool explainと実Jobで確認
```

## 1. 追加Node用の値を決める

例として次を使います。

| 項目 | 例 |
|---|---|
| Cluster | `home` |
| 追加Node名 | `node-b` |
| Controller listen | `192.168.1.10:8443` |
| Controller URL | `https://192.168.1.10:8443` |
| Node state | `/var/lib/runnerloom/node-b` |
| VM disk | `/var/lib/libvirt/images/runnerloom-home-node-b` |
| VM network | `172.30.241.0/24` |
| Image cache | `100 GiB` |

NodeごとのVM network CIDRは、既存LAN、VPN、container network、他のRunnerLoom networkと重ならないprivate `/24`を選びます。

Controller側:

```bash
export RL_CONTROLLER_STATE="/var/lib/runnerloom/controller"
export RL_CONTROLLER_LISTEN="192.168.1.10:8443"
export RL_CONTROLLER_URL="https://192.168.1.10:8443"
export RL_NEW_NODE="node-b"
```

追加Node側では、まだ`runnerloom`のpathを変数へ保存しません。CLIのインストール後に`RL_BINARY`を設定します。

```bash
export RL_NODE_NAME="node-b"
export RL_NODE_STATE="/var/lib/runnerloom/node-b"
export RL_DISK_DIR="/var/lib/libvirt/images/runnerloom-home-node-b"
export RL_NETWORK_CIDR="172.30.241.0/24"
export RL_CONTROLLER_URL="https://192.168.1.10:8443"
```

## 2. ControllerをLANから到達可能にする

1台構成の`127.0.0.1:8443`は別PCから到達できません。Controller serviceを、信頼するLAN interfaceのIPで再生成します。

同じstateを使うControllerが起動中だとprocess lockにより更新を拒否するため、先に停止します。

```bash
sudo systemctl stop runnerloom-controller.service
sudo runnerloom service install \
  --role controller \
  --state "$RL_CONTROLLER_STATE" \
  --binary "$(command -v runnerloom)" \
  --listen "$RL_CONTROLLER_LISTEN" \
  --advertise "$RL_CONTROLLER_URL" \
  --start
```

既存の1台目Nodeの`agent.json`にある`controller`も、同じ`RL_CONTROLLER_URL`へ変更し、Agentを再起動します。

```bash
sudoedit /var/lib/runnerloom/node-a/agent.json
sudo systemctl restart runnerloom-agent-node-a.service
```

確認します。

```bash
sudo journalctl -u runnerloom-controller.service -n 100 --no-pager
sudo curl --silent --show-error --output /dev/null \
  --cacert "$RL_CONTROLLER_STATE/ca.pem" \
  "$RL_CONTROLLER_URL/"
```

Controllerのroot pathはHTTP 404でも構いません。`curl`が終了コード0なら、CAで検証したTLS接続とHTTP応答までは成功しています。NodeからTCP/8443へ到達できない場合は、Controller hostのinterface、host firewall、VLANを確認してください。インターネットへのport forwardingは不要です。

> [!WARNING]
> `--listen 0.0.0.0:8443`は全interfaceで待ち受けます。必要性とhost firewallを確認できる場合だけ使い、可能ならLAN IPを明示してください。

## 3. Cluster設定へ追加Nodeを登録する

RunnerLoomが配置先として認識するには、ControllerのCluster設定にNodeが必要です。初回構築で使った`cluster.json`を正本として更新します。

`nodes[]`へ次のような要素を追加し、利用させるPool名を`allowedPools`へ入れます。

```json
{
  "name": "node-b",
  "budget": {
    "vcpu": 12,
    "memoryMiB": 24576,
    "diskGiB": 300
  },
  "localCeiling": {
    "vcpu": 12,
    "memoryMiB": 24576,
    "diskGiB": 300
  },
  "allowedPools": [
    "linux-lite"
  ]
}
```

`budget`と`localCeiling`は追加PCの物理資源を超えない値にし、Host OS用の余裕を残します。

```bash
runnerloom config validate --file cluster.json --json

plan_id="$(sudo runnerloom config plan \
  --state "$RL_CONTROLLER_STATE" \
  --file "$PWD/cluster.json" \
  --json | jq -r .data.id)"

sudo runnerloom config apply \
  --state "$RL_CONTROLLER_STATE" \
  --plan "$plan_id" \
  --non-interactive --json

sudo systemctl restart runnerloom-controller.service
```

> [!CAUTION]
> 初回を対話式setupだけで作り、正本の`cluster.json`を残していない場合は、現在のNode、Pool、Image、Repository許可を正確に再構成して`config validate`とplan内容を確認してください。要素の省略を削除指示として使わないでください。

## 4. 追加Nodeを準備する

追加NodeへRunnerLoomをインストールし、KVM/libvirtを準備します。詳細は [INSTALL.md](INSTALL.md) と [QuickstartのHost準備](QUICKSTART.ja.md#1-hostを準備する) を参照してください。

```bash
sudo apt-get update
sudo apt-get install -y \
  qemu-kvm qemu-utils \
  libvirt-daemon-system libvirt-clients \
  cloud-image-utils nftables dnsmasq-base \
  ca-certificates curl jq
sudo systemctl enable --now libvirtd
runnerloom doctor --strict

export RL_BINARY="$(command -v runnerloom)"
test -n "$RL_BINARY"
```

`RL_BINARY`はCLIをインストールした後に設定します。空のままservice installへ渡さないでください。

Node固有設定を作ります。

```bash
sudo install -d -m 0700 "$RL_NODE_STATE"
sudo install -m 0600 /dev/null "$RL_NODE_STATE/agent.json"
sudoedit "$RL_NODE_STATE/agent.json"
```

```json
{
  "node": "node-b",
  "cluster": "home",
  "controller": "https://192.168.1.10:8443",
  "stateDir": "/var/lib/runnerloom/node-b",
  "diskDir": "/var/lib/libvirt/images/runnerloom-home-node-b",
  "networkCIDR": "172.30.241.0/24",
  "ceiling": {
    "vcpu": 12,
    "memoryMiB": 24576,
    "diskGiB": 300
  },
  "cacheGiB": 100,
  "qemuUser": "libvirt-qemu"
}
```

`controller`は、次の手順で招待へ書くURLと **完全に同じ文字列** にします。

## 5. Controllerで期限付き招待を作る

Controller側:

```bash
sudo install -d -m 0700 /var/lib/runnerloom/invitations
sudo runnerloom node invite \
  --state "$RL_CONTROLLER_STATE" \
  --url "$RL_CONTROLLER_URL" \
  --out "/var/lib/runnerloom/invitations/$RL_NEW_NODE.json"
```

招待fileには一回限りの秘密とController CA fingerprintが含まれます。信頼できる経路で追加Nodeへ渡し、参加完了後に両側から削除します。公開chat、Issue、shell historyへ内容を貼らないでください。

追加Nodeではprivate directoryへ保存します。

```bash
sudo install -d -m 0700 /var/lib/runnerloom/invitations
sudo install -m 0600 node-b.json \
  /var/lib/runnerloom/invitations/node-b.json
```

## 6. 参加申請を承認する

### 6-1. 追加Nodeから申請する

```bash
sudo runnerloom setup \
  --role node \
  --file "$RL_NODE_STATE/agent.json" \
  --invitation /var/lib/runnerloom/invitations/node-b.json \
  --apply --non-interactive --json
```

最初は`PendingApproval`となり、request IDとCSR hashが表示されます。

### 6-2. Controllerでfingerprintを照合する

```bash
sudo runnerloom node pending --state "$RL_CONTROLLER_STATE"
```

別経路で確認した追加NodeのCSR hashと一致することを確認し、表示されたIDを承認します。

```bash
sudo runnerloom node approve REQUEST_ID \
  --state "$RL_CONTROLLER_STATE"
```

名前だけ、MAC addressだけ、自動発見結果だけで承認しないでください。

### 6-3. 追加Nodeで証明書を受け取る

承認後、追加Nodeで **同じコマンド** を再実行します。

```bash
sudo runnerloom setup \
  --role node \
  --file "$RL_NODE_STATE/agent.json" \
  --invitation /var/lib/runnerloom/invitations/node-b.json \
  --apply --non-interactive --json
```

`Approved`となり、`node.pem`、`node-key.pem`、`ca.pem`、`ca.sha256`がNode stateへ保存されることを確認します。

```bash
sudo ls -l "$RL_NODE_STATE"
```

Node private keyを別PCへ複製して使い回さないでください。

## 7. 追加NodeのnetworkとAgentを起動する

```bash
sudo runnerloom network plan --config "$RL_NODE_STATE/agent.json"
sudo runnerloom network apply --config "$RL_NODE_STATE/agent.json"
sudo runnerloom network check --config "$RL_NODE_STATE/agent.json"
```

Node state（その下にimage cacheが作られます）とVM disk directoryが同じfilesystemか確認します。

```bash
sudo mkdir -p "$RL_DISK_DIR"
findmnt -T "$RL_NODE_STATE"
findmnt -T "$RL_DISK_DIR"
```

Agent serviceを入れます。`RL_BINARY`が現在のCLIを指していることを直前にも確認します。

```bash
export RL_BINARY="$(command -v runnerloom)"
test -n "$RL_BINARY"

sudo runnerloom service install \
  --role agent \
  --state "$RL_NODE_STATE" \
  --config "$RL_NODE_STATE/agent.json" \
  --binary "$RL_BINARY" \
  --start
```

```bash
systemctl --no-pager --full status "runnerloom-agent-$RL_NODE_NAME.service"
sudo journalctl -u "runnerloom-agent-$RL_NODE_NAME.service" -n 100 --no-pager
```

初回はControllerからImageを取得してdigestを確認します。`IMAGE_MISSING`が解消するまで空VMを起動することはありません。

## 8. Controllerから配置状態を確認する

Controller側:

```bash
sudo runnerloom node list --state "$RL_CONTROLLER_STATE"
sudo runnerloom pool explain linux-lite --state "$RL_CONTROLLER_STATE"
sudo runnerloom status --state "$RL_CONTROLLER_STATE"
```

追加Nodeに次の拒否理由が残っていないことを確認します。

- `NODE_NOT_READY`
- `NETWORK_NOT_VERIFIED`
- `LOCAL_CEILING`
- `IMAGE_MISSING`
- `INSUFFICIENT_BUDGET`
- `INSUFFICIENT_PHYSICAL_DISK`

Poolの`allowedPools`、Node budget、Image digest、network、heartbeatを順に確認してください。

## 9. 実Jobで確認する

最初は追加Nodeだけを許可する一時Poolを作るか、他Nodeをdrainして配置先を明確にします。

```bash
sudo runnerloom node drain node-a --state "$RL_CONTROLLER_STATE"
```

[Quickstartのsmoke workflow](QUICKSTART.ja.md#7-最初のgithub-actions-jobを実行する) を実行し、`runner_name`、Node journal、VM状態から`node-b`へ配置されたことを確認します。Job後にVMが`Deleted`となったら、1台目をresumeします。

```bash
sudo runnerloom node resume node-a --state "$RL_CONTROLLER_STATE"
```

## LAN自動発見を使う場合

自動発見は任意であり、認証の代わりではありません。Controllerと探索側へAvahiを入れます。

```bash
sudo apt-get install -y avahi-daemon avahi-utils
```

Controller serviceを、LANから到達できる`.local` URLで再生成します。Controllerを止め、すべてのNodeの`agent.json`にある`controller`も同じURLへ変更してください。

```bash
sudo systemctl stop runnerloom-controller.service
sudo runnerloom service install \
  --role controller \
  --state "$RL_CONTROLLER_STATE" \
  --binary "$(command -v runnerloom)" \
  --listen "$RL_CONTROLLER_LISTEN" \
  --advertise https://controller.local:8443 \
  --discoverable --start
```

各Agentを再起動した後、探索側で確認します。

```bash
runnerloom discover --json
```

表示された候補は未認証です。招待fileに含まれるCA fingerprintと、TLSで検証したController名を信頼基準にしてください。

## DHCPでControllerのIPが変わる場合

- 安定したLAN DNS名または`.local`名を`advertise`に使う
- Host firewallで8443/tcpを信頼LANへ限定する
- IP固定の`--listen`を使う場合は、address変更時にunitを再生成する
- `agent.json`の`controller`、招待URL、Controllerの`--advertise`を一致させる

別VLAN・別拠点は自動発見対象外です。到達可能なHTTPS URLを明示し、routing、firewall、TLS名を個別に確認してください。
