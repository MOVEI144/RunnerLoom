# 対話CLI

RunnerLoomは、既存のサブコマンドとJSON出力を維持したまま、人が端末から操作するための対話メニューを提供します。

[ドキュメント一覧へ戻る](README.md) · [1台構成Quick Start](QUICKSTART.ja.md) · [運用](OPERATIONS.ja.md) · [Cache管理](CACHE.ja.md)

## 起動

stdin・stdout・stderrがすべて端末へ接続されている場合、引数なしで起動すると対話メニューへ入ります。

```bash
runnerloom
```

明示的に起動する場合は次を使います。

```bash
runnerloom interactive
```

現在とは異なるController stateを使う場合は、通常のpersistent flagを指定できます。

```bash
runnerloom --state /var/lib/runnerloom/controller interactive
```

パイプ、リダイレクト、CI、systemdなど端末ではない環境で引数なし実行した場合は、入力待ちせず従来どおりhelpを表示します。

## 非対話CLIとの互換性

次の既存操作は変更されません。

```bash
runnerloom status --state /var/lib/runnerloom/controller
runnerloom cache status --config /var/lib/runnerloom/node-a/agent.json --json
runnerloom setup --file cluster.json --apply --non-interactive
```

以下の条件では対話メニューを自動起動しません。

- サブコマンドを指定した
- `--json`を指定した
- `--non-interactive`を指定した
- stdin、stdout、stderrのいずれかが端末ではない

自動化では、従来どおりサブコマンド、必要なflag、`--non-interactive`、必要に応じて`--json`を明示してください。

## メニュー構成

トップメニューから次を選べます。

| メニュー | 主な操作 |
|---|---|
| 初期セットアップ | 既存の対話式`setup`を起動 |
| 状態・診断 | `doctor`、全体状態、Node、Pool、GitHub接続 |
| Image cache | status、完全検証、prune dry-run/apply、same-host seed |
| VM network | plan、check、確認後のapply |
| Node管理 | list、pending、invite、approve、join、drain、resume、revoke |
| Pool・GitHub | Pool一覧、配置理由、GitHub権限検査 |
| Backup・maintenance | DB backup、compact dry-run/apply |
| systemd service | ControllerまたはAgent unitのinstall/update |

メニュー内で実行される処理は、対応する既存Cobraサブコマンドと同じです。対話モード専用の別実装でController DBやhostを直接変更しません。

## 破壊的・host変更操作

次のような操作は、対象、dry-runまたは計画、停止条件を表示した後、指定された確認語を完全一致で入力した場合だけ実行します。

- Cache pruneの`--apply`
- VM networkの`apply`
- Cache seed
- Node approve、drain、resume、revoke
- Node join
- Backup作成
- DB compactの`--apply`
- systemd service install/update

例:

```text
続行するには PRUNE と正確に入力:
```

`prune`、`Prune`、空Enterなどは不一致として扱い、変更しません。Cache pruneとDB compactは対話側でも先にdry-runを実行します。対象serviceが動作中、所有関係が不明、VM状態が不明など、既存コマンドのfail-closed条件はそのまま維持されます。

## 入力の扱い

- 入力値はシェル文字列へ連結せず、引数の配列として既存CLIへ渡します。
- 空白や引用符を含むpathをシェル再解釈しません。
- 1入力は4096 bytesに制限します。
- 招待secret、JIT config、private key、PAT本文の入力や表示は行いません。対話メニューでは、それらを保存したprivate fileのpathだけを扱います。
- EOFまたは端末切断時は、新しい変更を開始せず対話モードを終了します。

## 初期セットアップの範囲

トップメニューの「初期セットアップ」は既存の`runnerloom setup`を呼び出します。`setup`成功だけでは、次はまだ完了していません。

- Golden Image import
- VM networkのplan/apply/check
- GitHub AppとRunner Groupの接続確認
- Controller/Agent serviceのinstall/start
- 実際のGitHub Actions Job
- Job後のVM削除確認

初回導入では[1台構成Quick Start](QUICKSTART.ja.md)の完了条件も確認してください。

## トラブル時

対話操作が失敗した場合は`[未完了]`と理由を表示し、メニューへ戻ります。原因調査や再現ログが必要な場合は、同じ操作を通常のサブコマンドと`--json`で実行できます。

```bash
runnerloom cache status \
  --config /var/lib/runnerloom/node-a/agent.json \
  --verify --json
```

端末判定に失敗する特殊なconsoleでは、対話モードを強制するflagは提供しません。通常のサブコマンドを使用してください。これはCIやリダイレクト先で誤って入力待ちになることを防ぐためです。
