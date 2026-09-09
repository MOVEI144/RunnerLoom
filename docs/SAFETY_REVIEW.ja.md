# 資源予約・ネットワーク検証の回帰試験

2026-09-09。対象はCPU/LAN用ランタイムです。GPUや未審査の公開PRを安全に実行できるという認定ではありません。

## 修正した問題

### 専用枠を、片付け途中に一般用途へ貸してしまう

停止したVMのCPU・RAMだけ返却し、ディスク削除がまだ終わっていない場合、旧計算では専用枠のCPU・RAMが一般Poolから利用できました。

修正後は、予約全量からその予約のVMが保持する量を引いた残りを保護します。全量の返却確認までは新しい専用VMにもそのスロットを再使用しません。CPUだけ空いていても、まだ残っているディスクやGPUが消えたとは扱いません。

### 使用中の予約の移動・付け替え

予約名だけで使用台数を数えていたため、予約を別Nodeや別Poolへ変更しても、古いVMが新しい予約を消費していると誤判定できました。

修正後は名前・Node・Poolの所有関係を検査し、保持中の資源がある場合は移動・付け替えを拒否します。不整合な復旧データでも新規割り当てを止めます。CPU・RAMだけ返却済みの予約を無視した提供上限の縮小も拒否します。

### 同じUUIDなら、ネットワーク設定変更を見逃す

UUIDと所有情報が一致しても、NATがルーティングへ変更されたり、IPv6やVM間通信が許可されたりする可能性があります。

修正後は、NAT方式、IPv6、VM間隔離、ブリッジ、ゲートウェイ、DHCP範囲、追加IP・ルートも確認します。libvirtが生成するMACやUUID、既定の省略表記とは区別します。相違がある場合は新規VM作成を止め、管理者へ調査を要求します。既存ネットワークを無断で置き換えません。

### ホスト宛ルートの衝突

`172.30.240.91`のようにCIDR表記でないホスト宛ルートも、IPv4では/32としてVM用サブネットとの衝突を検査します。

## 再現・確認方法

```sh
go test -race -count=1 ./...
go test ./internal/core -run '^$' -fuzz '^FuzzReservationProtection$' -fuzztime=10s
```

主な回帰テスト：

- `TestReservationSurvivesPartialCleanup`
- `TestReservationCannotMoveOrRetargetWhileHeld`
- `TestPartiallyReleasedReservationPreventsBudgetShrink`
- `TestReservationCorruptionBlocksNewAllocation`
- `TestNetworkSecurityDriftPreservesIdentityButMustFail`
- `TestHostRouteConflictsWithVMSubnet`

修正前に12件の問題ケースが失敗することを確認し、修正後は通過しています。Go 1.27.1のローカルテストとGitHub Actionsのrace/vet検証は区別して記録します。これらのネットワーク回帰試験は明示的なlibvirt/nftテスト代役を使い、実VM起動や実際のパケット遮断の証拠は別のCIジョブで確認します。
