# RunnerLoom運用ガイド

このページは、最初のGitHub JobとVM cleanupが成功した後の日常操作をまとめます。

[ドキュメント一覧へ戻る](README.md) · [1台構成](QUICKSTART.ja.md) · [更新・削除](INSTALL.md) · [トラブル対応](TROUBLESHOOTING.ja.md)

以下の例では、Controller stateを `/var/lib/runnerloom/controller`、Nodeを`node-a`、Node設定を `/var/lib/runnerloom/node-a/agent.json` とします。

```bash
export RL_CONTROLLER_STATE="/var/lib/runnerloom/controller"
export RL_NODE_NAME="node-a"
export RL_NODE_STATE="/var/lib/runnerloom/node-a"
export RL_NODE_CONFIG="$RL_NODE_STATE/agent.json"
```

## 日常の状態確認

最初にこの4つを見ると、GitHub、Node、Pool、VMのどこで止まっているかを分離できます。

```bash
sudo runnerloom status --state "$RL_CONTROLLER_STATE"
sudo runnerloom node list --state "$RL_CONTROLLER_STATE"
sudo runnerloom pool list --state "$RL_CONTROLLER_STATE"
sudo runnerloom vm list --state "$RL_CONTROLLER_STATE"
```

配置できない理由は`pool explain`で確認します。

```bash
sudo runnerloom pool explain linux-lite \
  --state "$RL_CONTROLLER_STATE"
```

serviceとjournal:

```bash
systemctl is-active runnerloom-controller.service
systemctl is-active "runnerloom-agent-$RL_NODE_NAME.service"

sudo journalctl -u runnerloom-controller.service -n 100 --no-pager
sudo journalctl -u "runnerloom-agent-$RL_NODE_NAME.service" -n 100 --no-pager
```

## 新しいJobの受付を止める

Nodeをdrainしても、実行中Jobは継続します。更新、保守、物理作業の前に使います。

```bash
sudo runnerloom node drain "$RL_NODE_NAME" \
  --state "$RL_CONTROLLER_STATE"
```

再開:

```bash
sudo runnerloom node resume "$RL_NODE_NAME" \
  --state "$RL_CONTROLLER_STATE"
```

`drain`後は`status`と`vm list`で、保持中のJobとVMがなくなったことを確認してからservice停止や再起動を行います。

## 特定Jobの停止を要求する

```bash
sudo runnerloom vm stop VM_ID \
  --state "$RL_CONTROLLER_STATE"
```

これは停止要求を保存します。CPU/RAMはホスト側で停止を確認した後、diskは削除を確認した後に解放されます。Controllerの要求だけで「VMが消えた」ことにはしません。

## VMの診断logを見る

```bash
sudo runnerloom vm logs VM_ID \
  --config "$RL_NODE_CONFIG"
```

Node上のserial logは上限付きです。Job成果物の永続保存先ではありません。残す必要があるbuild成果物、model、checkpointは、Job終了前にArtifactまたは外部storageへ保存してください。

## 設定を変更する

設定はplanとapplyを分けます。

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
```

apply後、GitHub/Pool設定を反映するためControllerを再起動します。

```bash
sudo systemctl restart runnerloom-controller.service
sudo runnerloom github check --state "$RL_CONTROLLER_STATE"
sudo runnerloom pool explain linux-lite --state "$RL_CONTROLLER_STATE"
```

### PoolサイズやImageを変えるとき

実行中PoolのCPU、RAM、Image digestをその場で置き換えるより、新しいPool名と`runnerName`を作り、smoke test後にWorkflowを移行すると境界が明確になります。

```text
home-linux-lite-v1  →  drain
home-linux-lite-v2  →  smoke test → production workflow
```

古いPoolをJSONから省略しても、暗黙削除の指示にはなりません。実行記録や所有権を確認せずに設定要素を消さない設計です。

## Backup

Controllerを止めた状態で一貫したbackupを取ります。`backup --out`は既存ファイルを上書きしないため、`mktemp -d`で毎回一意のprivate directoryを確保します。

```bash
sudo runnerloom node drain "$RL_NODE_NAME" \
  --state "$RL_CONTROLLER_STATE"

sudo systemctl stop "runnerloom-agent-$RL_NODE_NAME.service"
sudo systemctl stop runnerloom-controller.service

(
  set -euo pipefail
  backup_root="/var/backups/runnerloom"
  sudo install -d -m 0700 "$backup_root"
  backup_dir="$(sudo mktemp -d "$backup_root/$(date -u +%Y%m%dT%H%M%SZ)-XXXXXXXX")"
  backup="$backup_dir/controller.db"

  sudo runnerloom backup \
    --state "$RL_CONTROLLER_STATE" \
    --out "$backup"
  sudo test -s "$backup"
  printf 'saved: %s\n' "$backup"
)
```

このblockはbackup作成または存在確認に失敗すると非ゼロで終了し、`saved:`を表示しません。同じ秒に再実行しても、一意のdirectoryを確保します。

`backup`が保存するのはSQLite snapshotです。完全な復旧には次も必要です。

- `master.key`
- `ca.pem`と`ca-key.pem`
- binding / ownership関連file
- GitHub credential JSONと参照先private key
- 各Nodeのidentity、`agent.json`、sequence
- 使用Imageのdigest、manifest、再取得方法

backup後にserviceを起動します。

```bash
sudo systemctl start runnerloom-controller.service
sudo systemctl start "runnerloom-agent-$RL_NODE_NAME.service"
```

復旧時は、古いControllerを確実にfenceし、Node上の実VMとDBの記録を照合してください。DBだけを戻して実行中VMを存在しなかったことにしないでください。

## Image cacheの点検と整理

通常の状態確認では、ControllerとNodeを分けて確認します。

```bash
sudo runnerloom cache status --state "$RL_CONTROLLER_STATE" --cache-gib 100
sudo runnerloom cache status --config "$RL_NODE_CONFIG"
```

古いImageは自動削除されません。旧Poolを無効化し、実行中VMがなく、Nodeが新しいcatalogを受信した後でdry-runします。適用時は対象ControllerまたはAgentを停止します。

```bash
sudo runnerloom cache prune --config "$RL_NODE_CONFIG" --older-than 168h --json
```

参照検査、hard link、物理解放量、中断download、Controller側の整理は [CACHE.ja.md](CACHE.ja.md) を参照してください。

## Controller DBの安全な整理

`maintenance compact`は、期限切れplan、期限切れかつ未参照の招待、保持数を超えた古いaudit rowだけを候補にします。Instance、SDK inbox、enrollment、Image、disk、unknown host stateは対象外です。

### 1. 先にdry-runする

```bash
sudo runnerloom maintenance compact \
  --state "$RL_CONTROLLER_STATE" \
  --older-than 720h \
  --keep-audit 1000 \
  --json
```

`applied: false`のまま、削除候補数と保護対象数を確認します。

### 2. Controllerを止めてapplyする

```bash
sudo systemctl stop runnerloom-controller.service

sudo runnerloom maintenance compact \
  --state "$RL_CONTROLLER_STATE" \
  --older-than 720h \
  --keep-audit 1000 \
  --apply --json
```

完了時は次を確認します。

```text
applied: true
walCheckpointComplete: true
vacuumComplete: true
```

削除transaction後のcheckpointやVACUUMで失敗した場合、error detailsの`applied`を確認してください。`applied: true`なら、後段が失敗しても対象rowの削除はcommit済みです。

```bash
sudo systemctl start runnerloom-controller.service
```

## Agent停止後に所有VMを手動回収する

通常はAgentがController指示に従ってcleanupします。Agentを停止し、対象VMがRunnerLoom所有で停止済みだと確認できる場合だけ`vm recover`を使います。

```bash
sudo systemctl stop "runnerloom-agent-$RL_NODE_NAME.service"

sudo runnerloom vm recover VM_ID \
  --config "$RL_NODE_CONFIG"
```

この操作は所有metadataが一致する既知VMだけを対象にします。unknown state、実行中domain、他の管理者が作ったdomainを強制削除する用途ではありません。

処理後にAgentを起動すると、Controllerへ削除確認を返します。

```bash
sudo systemctl start "runnerloom-agent-$RL_NODE_NAME.service"
```

## Serviceの再生成

binary path、Controller listen/advertise URL、LAN discovery設定を変更した場合は、同じ`service install`を再実行してRunnerLoom所有unitを更新します。

Controllerの再生成中は同じstateを使う常駐processを止めます。起動中のままだとprocess lockにより拒否されます。

```bash
sudo systemctl stop runnerloom-controller.service
sudo runnerloom service install \
  --role controller \
  --state "$RL_CONTROLLER_STATE" \
  --binary "$(command -v runnerloom)" \
  --listen 127.0.0.1:8443 \
  --advertise https://127.0.0.1:8443 \
  --start
```

Agent unitのcommandを確実に更新するため、既存Agentも先に止めます。

```bash
sudo systemctl stop "runnerloom-agent-$RL_NODE_NAME.service"
sudo runnerloom service install \
  --role agent \
  --state "$RL_NODE_STATE" \
  --config "$RL_NODE_CONFIG" \
  --binary "$(command -v runnerloom)" \
  --start
```

同名unitがRunnerLoom管理外の場合は上書きしません。

## 更新

Release更新は [INSTALL.mdの更新手順](INSTALL.md#更新手順) に従います。最低限、次の順番を守ります。

```text
drain
  → Job/VMがゼロ
  → service停止
  → DB + key + Node stateをbackup
  → checksum確認済みbinaryをinstall
  → doctor / github check / pool explain
  → smoke Job
  → resume
```

## 障害時に保存する情報

秘密を除き、次を記録すると調査しやすくなります。共有前の除外事項とprivateな収集方法は [トラブルシューティング](TROUBLESHOOTING.ja.md#調査情報を保存する) を参照してください。
