# Image cacheの確認・整理・重複排除

RunnerLoomはJobごとにVMを作り直しますが、Golden Imageまで毎回作り直すわけではありません。検証済みqcow2をControllerと各Nodeへcacheし、Jobごとの小さなoverlayから起動します。

[ドキュメント一覧へ戻る](README.md) · [1台構成](QUICKSTART.ja.md) · [運用](OPERATIONS.ja.md) · [トラブル対応](TROUBLESHOOTING.ja.md)

> [!IMPORTANT]
> cacheの自動削除は行いません。qcow2 backing imageを誤って消すと実行中VMを壊すため、整理は常にdry-run、所有service停止、参照の再検査、明示的な`--apply`の順で行います。

## 保存される場所

1台構成の標準pathでは、同じGolden Imageが次の順で使われます。

```text
Image build結果
/var/lib/runnerloom/builds/ubuntu-runner.qcow2
        │ image import
        ▼
Controller配布用cache
/var/lib/runnerloom/controller/images/<SHA256>.qcow2
        │ AgentがmTLSで取得
        ▼
Node cache
/var/lib/runnerloom/node-a/images/<SHA256>.qcow2
        │ read-only hard link
        ▼
VM storage base
/var/lib/libvirt/images/runnerloom-home-node-a/base/<SHA256>.qcow2
        │ qcow2 backing image
        ▼
Job専用overlay
/var/lib/libvirt/images/runnerloom-home-node-a/<VM-ID>/root.qcow2
```

通常はController cacheとNode cacheが別ファイルです。Node cacheとVM storage baseは同じinodeを共有するため、Node stateとVM disk directoryを同じfilesystemへ置きます。

`cache seed`を使う1台構成では、Controller cache、Node cache、VM storage baseの3つを同じinodeにできます。

## 何がcache上限に含まれるか

Controllerの`--cache-gib`またはNode設定の`cacheGiB`は、そのcache directory内の次を数えます。

- 完成済み`<SHA256>.qcow2`
- 再開可能な`.partial-<SHA256>.qcow2`
- crash後に残ったRunnerLoom所有の一時import/seed file

次は別会計です。

- Image build出力
- Jobごとのroot overlayとscratch disk
- Controller DB、Node state、serial log
- GitHub Artifact、GitHub Actions cache
- npm、Cargo、Go module、Docker layerなどJob内の依存cache

RunnerLoomはcache上限に加えて、cache filesystemへ **2 GiBの安全余裕** を残します。上限に余裕があっても、物理空きが不足する場合はImport/downloadを拒否します。

## 状態を確認する

### Controller cache

```bash
export RL_CONTROLLER_STATE="/var/lib/runnerloom/controller"

sudo runnerloom cache status \
  --state "$RL_CONTROLLER_STATE" \
  --cache-gib 100
```

Controllerの上限は永続設定ではないため、`--cache-gib`にはその環境で採用した運用上限を指定します。

### Node cache

```bash
export RL_NODE_CONFIG="/var/lib/runnerloom/node-a/agent.json"

sudo runnerloom cache status \
  --config "$RL_NODE_CONFIG"
```

Nodeでは`agent.json`の`cacheGiB`、`stateDir`、`diskDir`を使用します。Node statusは次を照合します。

- Controllerから最後に受け取った`catalog.json`
- 削除完了していないinstance manifest
- VM storage内の実際の`root.qcow2`
- 各overlayが指すbacking image
- Node cacheと`diskDir/base`が同じinodeか
- partial file、未知のfile、symlink、壊れた所有境界

`safeToPrune: false`の場合は、警告を解決するまで`--apply`を拒否します。

### 完全性も読み直す

全ImageのSHA-256とqcow2構造を再検査する場合だけ`--verify`を付けます。大きなImageをすべて読み込むため、通常のstatusより時間とI/Oを使います。

```bash
sudo runnerloom cache status \
  --config "$RL_NODE_CONFIG" \
  --verify
```

検査する内容は次のとおりです。

- file名のdigestと実データのSHA-256が一致
- 通常fileでありsymlinkではない
- standalone、非暗号化qcow2
- 外部data fileや外部backing fileを持たない

## 古いcacheを安全に整理する

### Imageを利用対象から外す

現在有効なPoolが参照するImageは保護されます。古いImageを退役させる場合は、まず旧Poolを無効化し、新しいPoolで実Jobとcleanupを確認します。

```text
新Poolを追加
  → smoke Job
  → Workflowを新runnerNameへ移行
  → 旧Poolをenabled: falseへ変更
  → Controller再起動
  → 各Nodeが一度正常sync
  → 実行中・停止処理中VMがゼロ
  → cache prune dry-run
```

無効なPoolのImageはNode catalogへ新しく配布されません。ただし、旧Imageを使う未削除instanceまたは実overlayがある間は引き続き保護されます。

### Controller cacheのdry-run

```bash
sudo runnerloom cache prune \
  --state "$RL_CONTROLLER_STATE" \
  --cache-gib 100 \
  --older-than 168h \
  --json
```

`older-than`は最低24時間です。既定は7日です。dry-runではfileを変更せず、保護理由、削除候補、論理容量、物理解放見込みを表示します。

適用時はControllerを停止し、同じ条件で再実行します。

```bash
sudo systemctl stop runnerloom-controller.service

sudo runnerloom cache prune \
  --state "$RL_CONTROLLER_STATE" \
  --cache-gib 100 \
  --older-than 168h \
  --apply --json

sudo systemctl start runnerloom-controller.service
```

### Node cacheのdry-run

```bash
sudo runnerloom cache prune \
  --config "$RL_NODE_CONFIG" \
  --older-than 168h \
  --json
```

適用時は対象Agentを停止します。

```bash
node_name="$(sudo jq -er .node "$RL_NODE_CONFIG")"
sudo systemctl stop "runnerloom-agent-$node_name.service"

sudo runnerloom cache prune \
  --config "$RL_NODE_CONFIG" \
  --older-than 168h \
  --apply --json

sudo systemctl start "runnerloom-agent-$node_name.service"
```

Node pruneは、Node cache fileと対応する`diskDir/base` hard linkをまとめて削除します。実overlay、未削除instance、現在のcatalog、未知のstorage entryのいずれかが残るImageは削除しません。

## 1台構成で重複をなくす

ControllerとNodeが同じHost、かつ両cacheが同じfilesystem上にある場合は、通常のHTTP downloadの代わりに明示的なhard linkを作れます。

Agentを停止し、ControllerへImport済みのdigestを指定します。

```bash
export RL_IMAGE_DIGEST="sha256:ACTUAL_64_HEX_DIGEST"

sudo systemctl stop runnerloom-agent-node-a.service 2>/dev/null || true

sudo runnerloom cache seed "$RL_IMAGE_DIGEST" \
  --config /var/lib/runnerloom/node-a/agent.json \
  --source-state /var/lib/runnerloom/controller
```

このコマンドは次を確認してから公開します。

- Controller cacheのSHA-256
- standalone qcow2構造
- Node cache上限と2 GiBの安全余裕
- Agentが停止していること
- 送信元とNode cacheが同じfilesystemであること

別filesystemの場合はcopyへ自動fallbackせず失敗します。その場合はAgentを起動し、通常のmTLS downloadを使ってください。

hard link後は、Node側だけをpruneしてもController側のlinkが残るため、物理容量は解放されない場合があります。出力は`removedLogicalBytes`と`reclaimedPhysicalBytes`を分けて表示します。

## 中断downloadの再開

Node downloadはdigestごとのowner-only partial fileを残します。

```text
<node-state>/images/.partial-<SHA256>.qcow2
```

次の同期ではHTTP `Range`とdigestを含む`If-Range`を使い、Controllerの同じImageから続きだけを取得します。完成時に全体SHA-256とqcow2構造を再検証し、成功後にatomic renameします。

- network切断やtimeout: partialを保持して再開
- ETag、Content-Range、全体サイズの不一致: 公開しない
- 最終SHA-256またはqcow2検査失敗: partialを破棄
- 古いpartial: Agent停止後の`cache prune`候補

## Job内の依存cache

RunnerLoom VMはJob終了時に削除されるため、次は自動では残りません。

```text
~/.cache
~/.npm
~/go/pkg/mod
~/.cargo
/var/lib/docker
node_modules
compiler object
```

必要に応じて、Workflow側でGitHub Actions cache、Artifact、container registry、外部object storageを使います。全Jobへ必ず必要なtoolchainはGolden Imageへ入れる方が起動後のdownloadを減らせます。

## 自動削除を行わない理由

容量超過時にLRUでImageを勝手に消す方式は採用していません。参照判断を誤ると、実行中overlayのbacking image、再起動復旧に必要なImage、未報告VMの所有証拠を壊すためです。

容量不足時は次のようにfail closedします。

- 新ImageのImport/downloadを拒否
- Nodeは対象digestを所持済みと報告しない
- ControllerはそのNodeへ`IMAGE_MISSING`を付けて配置しない
- 管理者がstatusとdry-runを確認してから整理する
