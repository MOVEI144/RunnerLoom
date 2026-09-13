# Contributing

PRを出す前に、このリポジトリのルートで `make check` を通してください。
テストの層、CIジョブ、緑のチェックが証明しないことは [テスト方針](docs/TESTING.ja.md) にあります。

## 必要なもの

- `go.mod` に書いてある Go
- 梱包だけ Python 3.12（`make package`。`PYTHON` で上書き可）
- `go.sum` の変更はコミットする

```bash
make check
```

`make check` は `gofmt`、`go vet`、`go test -race -count=1 ./...`、関連シェルの構文確認です。
実VM、Golden Image、`.deb` 梱包は含みません。

## 出す前の自分レビュー

テストが通ったあと、PRにする前に自分の差分を通読してください。レビュー待ちに出す前に、作者が最初のレビュー担当です。

確認すること:

- 頼んでいないファイルや整形だけのノイズが入っていない
- 秘密、招待、JIT、ホスト固有のパスや鍵が差分にない
- 失敗のテストが、直した契約を実際に押さえている
- 文書・`--help`・JSON が、まだやっていない操作を成功と書いていない
- mockで見たことと、実VMや実GitHubで見ていないことを分けて書ける

## 変更の種類と必須テスト

| 変えるもの | 最低限必要なもの |
|---|---|
| 設定・JSON・CLIの表示 | 既存の CLI / decode テスト。flag名は `runnerloom --help --json`、設定キーは `runnerloom config schema` が正 |
| 資源会計・予約・配置 | 再実行（replay）と競合（concurrent）のテスト |
| 停止・削除・network・cache prune | 所有していない資源を消さない否定テスト |
| libvirt / イメージ / 隔離 | CIの実VMジョブ、または `smoke-vm`。mockの `control` 統合テストを実VM合格と言わない |
| 文書 | `python3.12 scripts/check-docs.py` |

今の製品ホストとCIランナーは Ubuntu 24.04 x86_64 です。macOS も Windows も、ホストとしてはまだ資格がありません。将来対象にする可能性があるので、手元の都合で本番のパス検査、symlink拒否、OS前提を緩めないでください。

手元が macOS でも `go test ./...` は通る想定です。テスト専用の `TestMain` が `/var` の互換symlinkを避けます。これはテストハーネスだけです。

## やってはいけないこと

- 他人のホストで `network apply`、`service install`、実VMテストを、明示的な許可なしに実行する
- CI専用ラッパーをローカルで実VM経路として使う。オペレーター向けは `smoke-vm`
- 本番の GitHub 認証、招待秘密、JIT をテストやCIログに入れる
- 古いという理由だけで instance、inbox、enrollment、image、不明なホスト状態を消す。`maintenance compact` は dry-run が既定で、適用は Controller 停止中だけ
- JSONを書いただけでセットアップ成功と書く。未確認の操作は未確認と書く
- 脆弱性の公開Issueに秘密情報を載せる。報告先は [SECURITY.md](SECURITY.md)

## 文書

オペレーター向け手順は [docs/README.md](docs/README.md) から探します。
設計は [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)、合格済み証拠は [docs/VERIFICATION.md](docs/VERIFICATION.md) です。
