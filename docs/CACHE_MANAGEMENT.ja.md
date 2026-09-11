# Golden Image cacheの管理

RunnerLoomはJobごとにVMを作り直しますが、Golden Imageは再利用します。このページでは、Imageがどこへ保存され、何が自動削除されず、容量不足や破損時にどう安全に対処するかを説明します。

[ドキュメント一覧へ戻る](README.md) · [1台構成](QUICKSTART.ja.md) · [運用](OPERATIONS.ja.md) · [トラブル対応](TROUBLESHOOTING.ja.md)

> [!IMPORTANT]
> `cache prune`と`cache quarantine`は既定でdry-runです。`--apply`を付ける前に、対象のControllerまたはAgent serviceを停止する必要があります。RunnerLoomは、稼働中overlayのbacking Imageや所有不明のhard linkを自動削除しません。

## Cacheの全体像

```text
Golden Image build結果
/var/lib/runnerloom/builds/ubuntu-runner.qcow2
             │
             │ image import
             ▼
Controller配布用cache
<controller-state>/images/<SHA256>.qcow2
             │
             │ 認証済みmTLS download
             ▼
Nodeごとのprivate cache
<node-state>/images/<SHA256>.qcow2
             │
             │ 同じfilesystem上のread-only hard link
             ▼
VM storageのbase
<disk-dir>/base/<SHA256>.qcow2
             │
             │ qcow2 backing image
             ▼
Jobごとのoverlay
<disk-dir>/<VM-ID>/root.qcow2
```

`base/<SHA256>.qcow2`はNode cacheと同じinodeを指すため、その2つだけで容量が2倍になるわけではありません。一方、1台兼用構成でもController cacheとNode cacheは別の実体です。build結果も残す場合、同じImageが概ね3か所に存在します。

## 何が永続し、何が消えるか

| 対象 | Job後 | 用途 |
|---|---|---|
| Controller cache | 残る | Nodeへ配布する正本 |
| Node cache | 残る | 次のVM作成で再利用 |
| VM storageのbase hard link | 残る | qcow2 overlayのbacking Image |
| Jobのroot/scratch qcow2 | 停止と所有確認後に削除 | 1 Job専用の変更領域 |
| cloud-init seed、JIT設定 | 削除 | 1 Job専用の起動情報 |
| `~/.cache`、npm、Go、Cargo、Docker layer | VMとともに削除 | Job内cache |

依存パッケージをJob間で再利用する場合は、GitHub Actionsのremote cache、Artifact、registry cache、外部object storageを使用するか、信頼できる共通ツールをGolden Imageへ含めます。RunnerLoom自身は、Jobの書込み可能なhome directoryやDocker layerを次のJobへ引き継ぎません。

## Cache上限とfilesystem reserve

- Controller cacheの既定上限は`100 GiB`です。`image import --cache-gib`とcache管理コマンドの`--cache-gib`で指定します。
- Node cacheの上限は`agent.json`の`cacheGiB`です。
- Image import/download時は、cacheの論理上限だけでなく、同じfilesystemに最低`2 GiB`を残します。管理コマンドでは`--reserve-gib`で確認値を変更できます。
- VM作成時は、cacheとは別にPoolの要求diskと安全余裕を再確認します。

`cacheGiB`はbuild出力、ControllerとNodeの別cache、Job overlay、scratch、log、DB、Artifactを合算したホスト全体の上限ではありません。`cache status`では、cache使用量と実filesystemの空き、次のimportに使用できる小さい方のheadroomを表示します。

## Controller cacheを確認する

軽量な確認では、全Imageの再hashを行いません。

```bash
sudo runnerloom cache status \
  --state /var/lib/runnerloom/controller \
  --cache-gib 100 \
  --json
```

完全検証:

```bash
sudo runnerloom cache verify \
  --state /var/lib/runnerloom/controller \
  --cache-gib 100 \
  --json
```

`verify`は各Imageについて次を確認します。

- ファイル名とSHA-256が一致する
- symlinkやdeviceではなく通常ファイルである
- standaloneの非暗号化qcow2である
- 外部data fileや外部backing fileを持たない
- hard link数がRunnerLoomの把握範囲内である

Controller cacheでは、現在のCluster設定と未完了Instanceが参照するdigestを保護します。

## Node cacheを確認する

```bash
sudo runnerloom cache status \
  --node-config /var/lib/runnerloom/node-a/agent.json \
  --verify \
  --json
```

Nodeでは、次を突き合わせます。

- Controllerから受け取った現在のImage catalog
- `instances/*.json`の未削除VM manifest
- `<disk-dir>/base`のhard link
- 実際に残る`root.qcow2`のbacking Image
- inodeとhard link数

`references`が空で、完全検証が成功し、hard linkの所有関係も一致したImageだけが`prunable: true`になります。

## 安全に未参照Imageを整理する

### 1. dry-run

Controller:

```bash
sudo runnerloom cache prune \
  --state /var/lib/runnerloom/controller \
  --cache-gib 100 \
  --json
```

Node:

```bash
sudo runnerloom cache prune \
  --node-config /var/lib/runnerloom/node-a/agent.json \
  --json
```

確認する項目:

- `candidateEntries`
- `reclaimableBytes`
- 各Imageの`references`
- `staleBaseLink`
- `blockedReason`
- `unsafeEntries`と`invalidEntries`

### 2. 対象serviceを停止する

Controller cache:

```bash
sudo systemctl stop runnerloom-controller.service
```

Node cache:

```bash
sudo systemctl stop runnerloom-agent-node-a.service
```

Node名が異なる場合は、実際に生成されたunit名へ置き換えてください。`--apply`はprocess lockを取得できない場合、削除せず失敗します。

### 3. apply

Controller:

```bash
sudo runnerloom cache prune \
  --state /var/lib/runnerloom/controller \
  --cache-gib 100 \
  --apply --json
```

Node:

```bash
sudo runnerloom cache prune \
  --node-config /var/lib/runnerloom/node-a/agent.json \
  --apply --json
```

Node pruneは、未参照になったRunnerLoom所有のbase hard linkを先に再検証して外し、cache inodeのlink数が1になったことを確認してからcache fileを削除します。active/unknown VM、異なるinode、余分なhard link、壊れたmanifest、確認できないoverlayがある場合は削除しません。

24時間を超えたRunnerLoom所有の`.import-*`と`.partial-*`一時fileも整理対象です。名前の分からないfile、quarantine、operatorが置いたfileは自動削除しません。

## 破損Imageをquarantineする

`cache verify`が`CACHE_INTEGRITY`を返した場合、まずserviceを停止します。対象digestをdry-runします。

```bash
sudo runnerloom cache quarantine \
  sha256:ACTUAL_64_HEX_DIGEST \
  --node-config /var/lib/runnerloom/node-a/agent.json \
  --json
```

active VMや所有不明hard linkがないことを確認してapplyします。

```bash
sudo runnerloom cache quarantine \
  sha256:ACTUAL_64_HEX_DIGEST \
  --node-config /var/lib/runnerloom/node-a/agent.json \
  --apply --json
```

破損fileは次へ移動し、削除しません。

```text
<cache-dir>/quarantine/<digest>-<timestamp>-<id>.qcow2.bad
```

Controller cacheの場合は`--node-config`を省略します。Node serviceを再起動すると、現在のcatalogに必要なImageはControllerから再取得されます。Controller cacheをquarantineした場合は、検証済みの元Imageを`image import`で再登録してからControllerを起動してください。

validなImage、active VMに参照されるImage、想定外のhard linkがあるImageはquarantineを拒否します。

## Cacheが満杯になった場合

新しいImageのimport/downloadは、既存Imageを勝手に消さずに失敗します。Nodeは必要digestを持つまで`IMAGE_MISSING`となり、そのNodeへの新規配置を行いません。

1. `cache status --verify`で使用量と参照を確認
2. `cache prune`をdry-run
3. 対象serviceを停止
4. `cache prune --apply`
5. 必要なら`cacheGiB`または保存filesystemを見直す
6. serviceを起動し、`pool explain`で`IMAGE_MISSING`解消を確認

設定からImage名を省略しただけで安全に削除できるとは限りません。実行記録、Node catalog、base hard link、overlayの全てが未参照であることをRunnerLoomが確認します。

## 現在の制約

- ControllerとNodeが同じPCでも、両cacheは自動deduplicateしません。
- Node間でpeer-to-peer配布は行わず、各NodeがControllerから取得します。
- 中断downloadのbyte-range再開はまだ行わず、失敗時は一時fileを破棄して再取得します。
- quarantineの自動期限削除は行いません。
- writableなJob dependency cacheを次のJobへ引き継ぎません。

これらは、誤った共有やbacking Imageの消失より、安全側へ倒した制約です。
