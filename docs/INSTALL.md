# RunnerLoomのインストール・更新・削除

このページで扱うのは **CLIの配置、checksum確認、更新、削除** です。GitHub App、Golden Image、VM network、Controller/Agent serviceの設定は [1台構成セットアップ](QUICKSTART.ja.md) で行います。

[ドキュメント一覧へ戻る](README.md)

> [!IMPORTANT]
> RunnerLoomをインストールしただけでは、serviceは起動せず、Nodeは登録されず、libvirt networkやfirewallも変更されません。

## 対応環境

| 項目 | 現在の対応範囲 |
|---|---|
| Host OS | Ubuntu 24.04 |
| Architecture | x86_64 / amd64 |
| Guest | CPU用Ubuntu VM |
| Package | Linux/amd64 archive または Debian package |
| Status | Release candidate。実環境での受入試験が必要 |

配布バイナリを使う場合、Goは不要です。Windows/macOSホスト、GPU passthrough、Controller HAはこの版の対象外です。

## インストール手順

### 1. 同じReleaseから配布物を取得する

GitHubの [Releases](https://github.com/MOVEI144/RunnerLoom/releases) で、使用するtagを1つ選びます。次のファイルを **同じRelease** から取得してください。

- `runnerloom-...-linux-amd64.tar.gz` または `runnerloom_..._amd64.deb`
- `runnerloom-...-buildinfo.json`
- `SHA256SUMS`

Repositoryが非公開の場合、公開化する必要はありません。ログイン済みブラウザ、または認証済みGitHub CLIを使います。

```bash
gh auth status
gh release list --repo MOVEI144/RunnerLoom

# 上で選んだtagへ置換する
tag="vX.Y.Z"
mkdir -p "runnerloom-release-$tag"
cd "runnerloom-release-$tag"
gh release download "$tag" --repo MOVEI144/RunnerLoom
```

`gh`を使わない場合は、ブラウザから同じReleaseのassetを1つの空ディレクトリへ保存します。

### 2. checksumを確認する

ダウンロードしたディレクトリで実行します。

```bash
sha256sum --check --ignore-missing SHA256SUMS
```

インストールするarchiveまたは`.deb`に対して `OK` が必要です。`FAILED`、ファイル不足、別Releaseの混在がある場合はインストールしないでください。

`SHA256SUMS`は破損やRelease取り違えを検出します。Publisher自体の侵害まで防ぐものではありません。`buildinfo.json`にはsource commit、Go版、target、依存情報が記録されています。

### 3. `.deb` またはarchiveの片方だけを入れる

#### Debian packageを使う

ダウンロードディレクトリにRunnerLoomの`.deb`が1つだけあることを確認してから実行します。

```bash
ls -l runnerloom_*_amd64.deb
sudo apt install ./runnerloom_*_amd64.deb
```

CLIは通常 `/usr/bin/runnerloom` に入ります。packageにはserviceを自動起動するmaintainer scriptはありません。

#### Archiveを使う

```bash
tar -xzf runnerloom-*-linux-amd64.tar.gz
cd runnerloom-*-linux-amd64
./runnerloom version --json
sudo install -m 0755 runnerloom /usr/local/bin/runnerloom
```

CLIは `/usr/local/bin/runnerloom` に入ります。

> [!WARNING]
> `.deb`とarchiveを同時に入れないでください。通常は `/usr/local/bin` が `/usr/bin` より先に検索されるため、更新したつもりで古いarchive版を実行する原因になります。

### 4. 実行中のバイナリを確認する

```bash
command -v runnerloom
runnerloom version --json
runnerloom doctor
runnerloom --help --json > runnerloom-help.json
```

確認する点は次の3つです。

1. `command -v` が意図したinstall先を指している
2. `version --json` が選択したReleaseとsource commitを表示する
3. `doctor` が不足しているhost機能を明示する

`doctor`の失敗は、CLIのインストール失敗とは限りません。KVM、libvirt、qemu、nftablesなどのhost依存関係はQuickstartで準備します。

## 次に進む

初回構築は [QUICKSTART.ja.md](QUICKSTART.ja.md) を上から順に実行してください。

```text
CLI install
  ↓
Host dependency / KVM
  ↓
GitHub App + Runner Group
  ↓
Golden Image
  ↓
Cluster config + setup
  ↓
Network + service
  ↓
First GitHub Job
```

## インストールで変更されるもの

| 操作 | 変更するもの | 変更しないもの |
|---|---|---|
| `.deb` | `/usr/bin/runnerloom`、package文書 | systemd起動、Node登録、network、firewall |
| archive | 指定したbinary install先 | package DB、systemd、Node登録、network |
| `runnerloom version` / `doctor` | 読み取りと診断のみ | 設定、service、network |

## 更新手順

### 1. 新しいJobを止める

Controllerが動作している状態で、対象Nodeをdrainします。

```bash
sudo runnerloom node drain node-a \
  --state /var/lib/runnerloom/controller
sudo runnerloom status \
  --state /var/lib/runnerloom/controller
```

実行中Jobが終わり、保持中のVM資源がなくなったことを確認します。

### 2. serviceを止める

実際のunit名を先に確認します。

```bash
systemctl list-unit-files 'runnerloom-*'
sudo systemctl stop runnerloom-controller.service
sudo systemctl stop runnerloom-agent-node-a.service
```

Node名が異なる場合はunit名も置き換えてください。

### 3. offline backupを取る

`backup --out`は既存ファイルを上書きしません。更新のたびに新しい名前を使います。

```bash
sudo install -d -m 0700 /var/backups/runnerloom
backup="/var/backups/runnerloom/controller-$(date -u +%Y%m%dT%H%M%SZ).db"
sudo runnerloom backup \
  --state /var/lib/runnerloom/controller \
  --out "$backup"
printf 'saved: %s\n' "$backup"
```

DBだけでは完全な復旧backupになりません。次も別途、privateな保存先へ保全します。

- Controller master key、CA key/certificate、binding
- GitHub credential fileと参照先のprivate key
- 各Nodeのidentity、`agent.json`、sequence情報
- 使用中Imageのdigestと入手元

古いControllerと復元したControllerを同時に起動しないでください。Node上の実VMとController DBを照合せずにsnapshotを戻すことも避けてください。

### 4. 新Releaseを検証して入れる

このページ冒頭のdownloadとchecksum確認を繰り返し、現在と同じinstall方式で更新します。

```bash
runnerloom version --json
runnerloom doctor --strict
```

### 5. 起動して確認する

```bash
sudo systemctl start runnerloom-controller.service
sudo systemctl start runnerloom-agent-node-a.service

sudo runnerloom status --state /var/lib/runnerloom/controller
sudo runnerloom github check --state /var/lib/runnerloom/controller
sudo runnerloom pool explain linux-lite --state /var/lib/runnerloom/controller
```

最後に、秘密情報を使わない小さな手動Workflowを1回実行し、Job成功とVM削除を確認してからNodeをresumeします。

```bash
sudo runnerloom node resume node-a \
  --state /var/lib/runnerloom/controller
```

## Downgrade / rollback

対象版が現在のDB schemaを明示的にサポートする場合だけ、binary downgradeを行えます。それ以外は、古いControllerを確実にfenceし、一貫したoffline backupから復元してください。

失敗した更新であっても、実VMやNode報告が進んでいる可能性があります。DBだけを機械的に巻き戻して「Jobが存在しなかった」ことにしないでください。

## データを残して削除する

最初にdrainし、Job完了とVM cleanupを確認します。生成済みunitを停止・無効化した後でbinary/packageを削除します。

```bash
sudo systemctl disable --now runnerloom-controller.service
sudo systemctl disable --now runnerloom-agent-node-a.service

# Debian packageで入れた場合
sudo apt remove runnerloom

# Archiveで入れた場合
sudo rm /usr/local/bin/runnerloom
```

次のdirectoryは、自動では削除しません。

- `/var/lib/runnerloom/controller`
- `/var/lib/runnerloom/node-*`
- RunnerLoom専用のVM disk directory
- Image build/cache directory
- backup directory

所有VM、libvirt domain、network、firewall規則、backupを確認するまでstate directoryを再帰削除しないでください。libvirtの共有`default` networkや、他のアプリケーションが管理するfirewall規則も削除しないでください。
