# RunnerLoom クイックスタートガイド

RunnerLoom は、GitHub Actions のジョブ要求に応じて、あなた自身のマシン上に使い捨ての Ubuntu VM（仮想マシン）をプロビジョニングするソフトウェアです。ジョブは隔離された VM 内で実行され、完了後に VM とその作業ディスクは自動的に削除されます。

外部へのポート開放（ポートフォワーディング）や SSH の公開は不要です。複数台構成の場合、各 Node（ワーカー）から Controller の LAN 用 HTTPS ポートに到達できれば動作します。

> **注記**: まずは非公開リポジトリ（Private Repository）と、CPU のみを割り当てる Ubuntu VM で始めることを推奨します。現在のバージョンでは、GPU パススルー、Windows/macOS ホスト、Controller の自動 HA（高可用性）構成はサポートされていません。

配布物の入手・チェックサム検証・更新方法については、[Install / upgrade](INSTALL.md) を参照してください。まずは以下のコマンドでバージョンとソースコミットを確認します：

```bash
runnerloom version --json
```

---

## 1. 必要な環境の準備

ホスト OS は **Ubuntu Server 24.04 x86_64** を想定しています。BIOS/UEFI で仮想化支援機能（VT-x/AMD-V）を有効にしてください。CLI の配布バイナリを使用する場合、Go のインストールは不要です。

```bash
sudo apt-get update
sudo apt-get install -y qemu-kvm qemu-utils libvirt-daemon-system \
  libvirt-clients cloud-image-utils nftables ubuntu-keyring curl gpgv dnsmasq-base
sudo systemctl start libvirtd
sudo install -m 0755 runnerloom /usr/local/bin/runnerloom
runnerloom doctor
```

※ この `apt` 操作により、ホストに必要な仮想化パッケージがインストールされます。既存の仮想化環境と共存させる場合は、事前に設定とディスクの空き容量を確認してください。

ソースからビルドする場合は、リポジトリの `go.mod` に指定されたバージョンの Go を使用してください：

```bash
go build -trimpath -o runnerloom ./cmd/runnerloom
```

---

## 2. GitHub のアクセス許可設定

Organization の Settings から、利用する Runner Group を作成し、アクセス範囲を **Selected repositories** に設定します。対象となる非公開リポジトリのみを選択し、公開リポジトリの許可は無効にしてください。設定ファイルの `allowedRepositories` は、この GitHub 側のリストと一致させる必要があります。

認証には、専用の **GitHub App** の使用を推奨します。
1. Organization の `Self-hosted runners` 管理権限を付与し、Organization にインストールします。
2. リポジトリのメタデータを読み取る API 権限も必要です。
3. App の Client ID、Installation ID、秘密鍵（PEM）ファイルを用意してください。

```bash
sudo install -d -m 0700 /var/lib/runnerloom/controller
sudo install -m 0600 github-app.pem /var/lib/runnerloom/controller/github-app.pem
sudo editor /var/lib/runnerloom/controller/github-credentials.json
sudo chmod 0600 /var/lib/runnerloom/controller/github-credentials.json
```

`github-credentials.json` の設定例（ID は実際の値に置き換えてください）：

```json
{
  "clientID": "YOUR_GITHUB_APP_CLIENT_ID",
  "installationID": 123456,
  "privateKeyFile": "/var/lib/runnerloom/controller/github-app.pem"
}
```

※ PAT（Personal Access Token）を使用する場合も、CLI 引数や直接の設定ファイルへの書き込みは避け、`{"tokenFile":"/absolute/path/token"}` のように専用ファイルを参照させてください。最小権限の原則に従い、広範な権限を持つトークンの使い回しは避けてください。

---

## 3. ベースイメージのビルド

VM のベースとなるイメージを作成します。別の PC で作成し、完成した `qcow2` と `manifest` をコピーしてきても構いません。

```bash
sudo apt-get install -y libguestfs-tools
sudo install -d -m 0700 /var/lib/runnerloom/builds
sudo runnerloom image build \
  --out /var/lib/runnerloom/builds/ubuntu-runner.qcow2
```

このコマンドは、Canonical の署名を検証した Ubuntu クラウドイメージと、ハッシュ検証済みの公式 GitHub Runner を組み合わせてイメージを生成します。`--runner-version 2.x.y` でバージョンを固定することも可能です。

ビルドが完了したら、以下のファイルを確認してください：
- `/var/lib/runnerloom/builds/ubuntu-runner.qcow2`
- `/var/lib/runnerloom/builds/ubuntu-runner.qcow2.manifest.json`

マニフェストファイル内の `digest`（SHA-256）は後ほど使用します。手動で確認する場合は以下のコマンドを実行します：

```bash
sudo sha256sum /var/lib/runnerloom/builds/ubuntu-runner.qcow2
```

---

## 4. Pool とリソース上限の設定

```bash
runnerloom config sample > cluster.json
editor cluster.json
runnerloom config validate --file cluster.json
```

主に以下の項目を編集します：

| 設定項目 | 説明 |
|---|---|
| `github.url` | Organization の URL |
| `github.runnerGroupID` | 選択した Runner Group の数値 ID |
| `github.allowedRepositories` | Group に許可した非公開リポジトリのリスト |
| `images[].digest` | ビルドしたイメージの実際の SHA-256 ダイジェスト |
| `nodes[].budget` | この Node から VM に提供するリソースの合計上限 |
| `nodes[].localCeiling` | この Node の管理者として許可するリソースの絶対上限 |
| `pools[]` | VM 1台あたりの CPU・RAM・ディスクサイズと、最大同時実行数 |
| `reservations[]` | 特定の Pool 向けにあらかじめ確保（予約）する台数 |

※ サンプルの SHA-256 が `0` になっている箇所は、必ず実際の値に置き換えてください。RAM は MiB、ディスクは GiB 単位です。OS や管理プロセスが使用するリソースの余裕を残すように設定してください。

---

## 5. Controller と Node の兼任設定の適用

1台のマシンで Controller と Node の両方を実行する場合の設定を適用します。

```bash
sudo runnerloom setup \
  --state /var/lib/runnerloom/controller \
  --file "$PWD/cluster.json" \
  --role controller-node \
  --node node-a \
  --node-state /var/lib/runnerloom/node-a \
  --disk-dir /var/lib/libvirt/images/runnerloom-home-node-a \
  --advertise https://127.0.0.1:8443 \
  --apply --non-interactive
```

ここでは設定の保存と、Controller の CA、Node ID の初期化を行います。（この時点では、GitHub への接続確認や VM の起動確認はまだ完了していません）。

---

## 6. イメージの登録と VM ネットワークの構築

```bash
sudo runnerloom image import \
  --state /var/lib/runnerloom/controller \
  --file /var/lib/runnerloom/builds/ubuntu-runner.qcow2 \
  --digest sha256:ACTUAL_SHA256 \
  --cache-gib 100

sudo runnerloom network plan --config /var/lib/runnerloom/node-a/agent.json
sudo runnerloom network apply --config /var/lib/runnerloom/node-a/agent.json
sudo runnerloom network check --config /var/lib/runnerloom/node-a/agent.json
```

`ACTUAL_SHA256` は実際の64桁のハッシュに置き換えてください。

VM 用ネットワークは、デフォルトで `172.30.240.0/24` が使用されます。既存の LAN や VPN のルーティングと競合する場合は適用が拒否されるため、その場合は設定ファイルで別のプライベート IPv4 サブネットを指定してください。

---

## 7. 接続確認とサービスの起動（常駐化）

```bash
sudo runnerloom github check --state /var/lib/runnerloom/controller

sudo runnerloom service install \
  --role controller --state /var/lib/runnerloom/controller \
  --binary /usr/local/bin/runnerloom --start

sudo runnerloom service install \
  --role agent --state /var/lib/runnerloom/controller \
  --config /var/lib/runnerloom/node-a/agent.json \
  --binary /usr/local/bin/runnerloom --start
```

動作状況やログの確認は以下のコマンドで行います：

```bash
sudo runnerloom status --state /var/lib/runnerloom/controller
sudo runnerloom pool explain linux-lite --state /var/lib/runnerloom/controller
sudo journalctl -u runnerloom-controller -n 100 --no-pager
sudo journalctl -u runnerloom-agent-node-a -n 100 --no-pager
```

初回起動時は、Node が Controller からイメージを取得します。取得と検証が完了するまでは `IMAGE_MISSING` と表示されます。

---

## 8. GitHub Actions からの利用

対象リポジトリに `.github/workflows/home-test.yml` を作成し、テストを実行します。

```yaml
name: Home VM test
on:
  workflow_dispatch:
permissions:
  contents: read
jobs:
  test:
    runs-on: home-linux-lite
    timeout-minutes: 10
    steps:
      - name: Confirm environment
        run: |
          uname -a
          python3 --version
          git --version
          echo "Executed inside a disposable RunnerLoom VM"
```

`runs-on` には Pool の `runnerName` を指定します。GitHub の Actions タブから手動（`workflow_dispatch`）で実行し、正常に VM がプロビジョニングされて処理が完了することを確認してください。

---

## 9. 2台目（追加 Node）のセットアップ

複数台構成にする場合、Controller は Node から LAN 経由で到達可能な HTTPS アドレスで起動している必要があります。

Controller 側で招待用クレデンシャル（Invitation）を生成します：

```bash
sudo install -d -m 0700 /var/lib/runnerloom/invitations
sudo runnerloom node invite \
  --state /var/lib/runnerloom/controller \
  --url https://CONTROLLER_LAN_ADDRESS:8443 \
  --out /var/lib/runnerloom/invitations/node-b.json
```

生成された `node-b.json` を安全な方法で追加 Node に転送してください。（使用後は削除を推奨します）。

追加 Node 側で設定ファイルを準備し、セットアップを実行します：

```bash
sudo runnerloom setup --role node \
  --file /var/lib/runnerloom/node-b/agent.json \
  --invitation /var/lib/runnerloom/invitations/node-b.json \
  --apply --non-interactive
```

Controller 側で `runnerloom node pending` を実行して指紋（Fingerprint）を確認し、`runnerloom node approve ID` で承認します。承認後、追加 Node 側でもう一度同じ setup コマンドを実行すると、証明書が発行されて参加が完了します。

---

## 日常のオペレーションコマンド

```bash
# 新規ジョブの受付を停止する（実行中のジョブは継続）
sudo runnerloom node drain node-a --state /var/lib/runnerloom/controller

# ジョブの受付を再開する
sudo runnerloom node resume node-a --state /var/lib/runnerloom/controller

# 特定のジョブ（VM）を強制停止する
sudo runnerloom vm stop VM_ID --state /var/lib/runnerloom/controller

# 設定変更のプレビュー（Plan）
sudo runnerloom config plan --file "$PWD/cluster.json" --state /var/lib/runnerloom/controller --json

# 設定変更の適用（Apply）
sudo runnerloom config apply --plan PLAN_ID --state /var/lib/runnerloom/controller --non-interactive --json
```

**※ 注意**: 保持しておきたい学習モデルやビルドの成果物は、ジョブが終了して VM が破棄される前に、外部ストレージや GitHub Artifacts に保存するように Workflow を構成してください。
