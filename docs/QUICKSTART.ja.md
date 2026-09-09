# RunnerLoomを1台から使う

RunnerLoomは「GitHubが受け付けた仕事に合わせて、自分のPC上に使い捨てVMを作る」ソフトです。仕事はVM内で実行され、終了後はVMと作業ディスクを削除します。NodeへのSSH公開や、インターネットへのポート転送は不要です。複数台構成では、NodeからControllerのLAN用HTTPSポートへ到達できる必要があります。

まずは非公開Repositoryと、CPU用のUbuntu VMで始めてください。GPUの割り当て、Windows/macOSホスト、Controllerの自動二重化はこの版の対象外です。GitHub-hostedと同じソフトウェア全部入りのイメージではありません。

## 1. 必要なもの

ホストはUbuntu Server 24.04 x86_64を想定しています。BIOS/UEFIで仮想化支援を有効にしてください。CLIの配布バイナリを使う場合、Goのインストールは不要です。

```bash
sudo apt-get update
sudo apt-get install -y qemu-kvm qemu-utils libvirt-daemon-system \
  libvirt-clients cloud-image-utils nftables ubuntu-keyring curl gpgv dnsmasq-base
sudo systemctl start libvirtd
sudo install -m 0755 runnerloom /usr/local/bin/runnerloom
runnerloom doctor
```

このapt操作はホストにソフトウェアとサービスを追加します。RunnerLoomが黙って実行する処理ではありません。既存の仮想化基盤との兼用は、先に設定と空き容量を確認してください。

自分でビルドする場合はリポジトリのGo版に対応するツールチェーンで実行します。

```bash
go build -trimpath -o runnerloom ./cmd/runnerloom
```

## 2. GitHubの利用許可を用意する

OrganizationのSettingsから、利用するRunner Groupを作り、アクセスを **Selected repositories** にします。非公開の対象Repositoryだけを選び、公開Repositoryの許可を無効にしてください。設定ファイルの `allowedRepositories` は、GitHub側の一覧と一致させます。

認証には専用GitHub Appを推奨します。OrganizationのSelf-hosted runners管理権限を付け、そのOrganizationへインストールします。利用するAPIにはRepositoryのメタデータの読み取りも必要です。AppのClient ID、Installation ID、秘密鍵ファイルを用意してください。メンバー権限・プランで利用できるGroupの範囲はGitHub側の設定に従います。

```bash
sudo install -d -m 0700 /var/lib/runnerloom/controller
sudo install -m 0600 github-app.pem /var/lib/runnerloom/controller/github-app.pem
sudo editor /var/lib/runnerloom/controller/github-credentials.json
sudo chmod 0600 /var/lib/runnerloom/controller/github-credentials.json
```

`github-credentials.json` の内容例（IDは実際の値に置換）：

```json
{
  "clientID": "YOUR_GITHUB_APP_CLIENT_ID",
  "installationID": 123456,
  "privateKeyFile": "/var/lib/runnerloom/controller/github-app.pem"
}
```

PATを使用する場合も、本文をCluster設定やCLI引数に書きません。専用の0600ファイルを `{"tokenFile":"/absolute/path/token"}` で参照します。APIに必要な最小権限だけを付け、普段使いの広い権限のトークンの使い回しは避けてください。

## 3. VMの元イメージを作る

イメージ作成だけに必要な道具を入れます。別のPCで作成し、完成したqcow2とmanifestを持ってきても構いません。

```bash
sudo apt-get install -y libguestfs-tools
sudo install -d -m 0700 /var/lib/runnerloom/builds
sudo runnerloom image build \
  --out /var/lib/runnerloom/builds/ubuntu-runner.qcow2
```

このコマンドはCanonicalの署名を検証したUbuntu cloud imageと、SHA-256を検証した公式GitHub Runnerを使います。完成したイメージはまだGitHubに登録されておらず、App秘密鍵も含みません。`--runner-version 2.x.y` で版を固定できます。`latest`でも、実際に解決した版とハッシュをmanifestに記録します。

次を確認してください。

```text
/var/lib/runnerloom/builds/ubuntu-runner.qcow2
/var/lib/runnerloom/builds/ubuntu-runner.qcow2.manifest.json
```

manifestの `digest` を以降で使います。SHA-256を自分で計算することもできます。

```bash
sudo sha256sum /var/lib/runnerloom/builds/ubuntu-runner.qcow2
```

## 4. PoolとPCの提供上限を決める

```bash
runnerloom config sample > cluster.json
editor cluster.json
runnerloom config validate --file cluster.json
```

主に次を変更します。

| 項目 | 設定する内容 |
|---|---|
| `github.url` | OrganizationのURL |
| `github.runnerGroupID` | 選択したRunner Groupの数値ID |
| `github.allowedRepositories` | Groupに許可した非公開Repository一覧 |
| `images[].digest` | 作成したイメージの実際のSHA-256 |
| `nodes[].budget` | このPCからVMへ提供する合計上限 |
| `nodes[].localCeiling` | このPCの管理者として許可する上限 |
| `pools[]` | VM1台のCPU・RAM・ディスク、最大台数 |
| `reservations[]` | 特定Pool向けに取り置く台数 |

サンプルのSHAがすべて0なのは、必ず実物へ置換するためです。架空の値のままではイメージ検証を通りません。サンプルのNode予算も自分のPCに合わせます。RAMはMiB、ディスクはGiBです。OS・管理プロセス用の余裕を残してください。

専用枠が不要なら `reservations` を `[]` にして最初の設定を作れます。一度登録した要素をJSONから省略しても、後の適用時に暗黙削除はしません。

## 5. 1台兼用の設定を保存する

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

人間向けには `runnerloom setup` の対話形式も用意しています。対話形式でもネットワークやサービスを黙って変更しません。

ここでできるのは設定、ControllerのCA、同居NodeのIDの保存です。`githubReady: false`、`vmReady: false` は、まだ実接続・VM起動を確認していないという意味です。

## 6. イメージをControllerへ登録し、VM専用ネットワークを作る

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

`ACTUAL_SHA256` は実際の64桁へ置換します。VMネットワークは、標準では `172.30.240.0/24` です。既存LAN・VPN経路と重なる場合は拒否します。別のプライベートIPv4 `/24` をNode設定に選んでください。

VMから自宅LAN、ホスト管理機能、他のVMへの通信を制限します。NATだけを作って隔離済みとは扱いません。既存のホストIP、DNS、ルーターのDHCP設定は書き換えません。

NodeのイメージキャッシュとVMディスクは、この版では同じファイルシステム上に置いてください。読み取り専用の元イメージをhard linkで共有するためです。

## 7. 接続を確認して常駐させる

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

Controllerは専用ユーザー、Agentは信頼されたホスト管理プロセスとして起動します。Agentはlibvirtとファイアウォールを操作するため、現在の版では管理者権限を持ちます。GitHubのJobがホスト管理者として動くという意味ではありません。

```bash
sudo runnerloom status --state /var/lib/runnerloom/controller
sudo runnerloom pool explain linux-lite --state /var/lib/runnerloom/controller
sudo journalctl -u runnerloom-controller -n 100 --no-pager
sudo journalctl -u runnerloom-agent-node-a -n 100 --no-pager
```

初回はNodeがControllerからイメージを取得します。取得・SHA検証が済むまで `IMAGE_MISSING` と表示されます。失敗時に空のVMを起動することはありません。

イメージや認証ファイルをサービスインストール後に更新するときは、Controllerの専用ユーザーが読める所有者・権限を保ってください。秘密鍵を全ユーザー読み取り可能にはしないでください。

## 8. GitHubから使う

許可したRepositoryに `.github/workflows/home-test.yml` を作ります。

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

`runs-on` はPoolの `runnerName` です。Workflowを既定ブランチに保存し、Actionsの画面で手動実行します。初回のScale Set名が既に存在し、ローカルの所有記録がない場合は勝手に採用しません。自分が作成したものだと確認した場合だけ `runnerloom github adopt --pool linux-lite --id NUMBER` を使います。

## 9. 2台目を追加する

ControllerをLANでNodeから到達できるHTTPSアドレスで起動します。ルーターへのポート転送は不要です。既存の1台目Nodeの `controller` URLも、証明書の名前と合うものへ更新してください。URL・待受を変えたらサービスユニットを再生成します。

Controllerで期限付き招待を作成します。

```bash
sudo install -d -m 0700 /var/lib/runnerloom/invitations
sudo runnerloom node invite \
  --state /var/lib/runnerloom/controller \
  --url https://CONTROLLER_LAN_ADDRESS:8443 \
  --out /var/lib/runnerloom/invitations/node-b.json
```

招待ファイルには秘密が含まれます。信頼できる経路で追加PCへ渡し、使用後は削除してください。MACアドレスや、自動検出した名前だけでは信頼しません。

追加PCで `examples/node.json` を編集し、0600で保存します。CPU・RAM・保存先の設定はそのPCの管理者が承認する値です。

```bash
sudo runnerloom setup --role node \
  --file /var/lib/runnerloom/node-b/agent.json \
  --invitation /var/lib/runnerloom/invitations/node-b.json \
  --apply --non-interactive
```

Controllerの `node pending` で申請名と鍵の指紋を確認して、`node approve ID` を実行します。追加PCで同じ参加コマンドをもう一度実行すると、承認済み証明書を受け取ります。招待は標準10分で切れるので、その間に受け取りまで済ませます。

その後は1台目と同様に `network plan/apply/check` と `service install --role agent` を実行します。設定済みPoolのうちサイズが合うものが候補になり、許可・空き・予約・イメージを照合して配置されます。

## 日常の操作

```bash
# 新しい仕事の受付を止める。今の仕事は継続
sudo runnerloom node drain node-a --state /var/lib/runnerloom/controller

# 再開
sudo runnerloom node resume node-a --state /var/lib/runnerloom/controller

# 特定の仕事を中断。CPU/RAMはホスト停止確認後に返却
sudo runnerloom vm stop VM_ID --state /var/lib/runnerloom/controller

# Node側の診断ログ
sudo runnerloom vm logs VM_ID --config /var/lib/runnerloom/node-a/agent.json

# 設定変更は計画してから適用
sudo runnerloom config plan --file "$PWD/cluster.json" --state /var/lib/runnerloom/controller --json
sudo runnerloom config apply --plan PLAN_ID --state /var/lib/runnerloom/controller --non-interactive --json
```

Pool/GitHub接続の設定を変更したらControllerを再起動します。実行中VMは、それだけで停止しません。サイズやImageを変えるときは、新しいPool名を作ると環境を明確に分けられます。

残したいモデル・ビルド成果物はJob終了前にArtifactや外部ストレージへ保存してください。VMディスクは永続保存場所ではありません。長時間学習では定期的に途中保存し、GitHub側の実行・トークン期限も確認します。


## LAN自動発見とDHCPで変わるアドレス

自動発見は任意です。使うPCに `avahi-daemon` と `avahi-utils` を入れ、ControllerをLANへ到達できる `.local` 名で起動します。ホストのDHCP設定を固定IPへ書き換える必要はありません。

```bash
sudo apt-get install -y avahi-daemon avahi-utils
sudo runnerloom service install --role controller \
  --state /var/lib/runnerloom/controller \
  --listen 192.168.1.10:8443 --advertise https://controller.local:8443 \
  --discoverable --start
runnerloom discover --json
```

例のIPとホスト名は自分のLANへ置き換えてください。DHCPでControllerのIPが変わる場合は、LAN専用の待受を維持する方法（例: OS側のインターフェース方針とファイアウォールを確認した上で `0.0.0.0:8443`）を選びます。IP固定の `--listen` は変更後にはそのまま使えません。

表示される候補は **未認証** です。発見画面の指紋を信用するのではなく、Controllerから受け取った招待の指紋で接続先を確認します。同じ `.local` 名の解決先IPが変わっても、TLSでは元の名前とCAを引き続き検証します。別VLAN・別拠点は自動発見対象ではありませんが、到達できるHTTPS URLの明示指定は利用できます。

Node起動時はCPU数とRAMを実測し、承認上限が物理量を超えていないか再確認します。ハードウェアを減らした後や、別PCへ設定をコピーした場合は、設定を見直すまで実行を止めます。Nodeの秘密鍵そのものを別PCへコピーして使い回すことは禁止です。
