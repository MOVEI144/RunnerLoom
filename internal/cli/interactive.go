package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/MOVEI144/RunnerLoom/internal/core"
	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
)

const interactiveInputLimit = 4096

var interactiveDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type interactiveMenuItem struct {
	Key   string
	Label string
}

func isInteractiveTerminal(stream any) bool {
	file, ok := stream.(*os.File)
	if !ok {
		return false
	}
	fd := file.Fd()
	return isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd)
}

func (a *App) interactiveTerminal() bool {
	return isInteractiveTerminal(a.In) && isInteractiveTerminal(a.Out) && isInteractiveTerminal(a.Err)
}

func (a *App) shouldStartInteractive() bool {
	return !a.JSON && !a.NonInteractive && a.interactiveTerminal()
}

func (a *App) addInteractive(root *cobra.Command) {
	interactive := &cobra.Command{
		Use:   "interactive",
		Short: "人向けの対話メニューを明示的に開く",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if a.JSON || a.NonInteractive {
				return core.Fail("INTERACTIVE_DISABLED", "--json または --non-interactive と対話モードは併用できません", nil)
			}
			if !a.interactiveTerminal() {
				return core.Fail("TTY_REQUIRED", "対話モードには端末へ接続されたstdin・stdout・stderrが必要です", nil)
			}
			return a.runInteractive(c.Context())
		},
	}
	root.AddCommand(interactive)
}

func readInteractiveLine(r io.Reader) (string, error) {
	var b strings.Builder
	one := []byte{0}
	noProgress := 0
	for {
		n, err := r.Read(one)
		if n > 0 {
			noProgress = 0
			switch one[0] {
			case '\n':
				return strings.TrimSpace(strings.TrimSuffix(b.String(), "\r")), nil
			default:
				if b.Len() >= interactiveInputLimit {
					return "", errors.New("対話入力が長すぎます")
				}
				b.WriteByte(one[0])
			}
		} else if err == nil {
			noProgress++
			if noProgress > 100 {
				return "", io.ErrNoProgress
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) && b.Len() > 0 {
				return strings.TrimSpace(strings.TrimSuffix(b.String(), "\r")), nil
			}
			return "", err
		}
	}
}

func interactiveSafe(value string) string {
	return terminalSafeLog([]byte(value))
}

func (a *App) interactivePrompt(label, fallback string) (string, error) {
	if fallback == "" {
		_, _ = fmt.Fprintf(a.Err, "%s: ", interactiveSafe(label))
	} else {
		_, _ = fmt.Fprintf(a.Err, "%s [%s]: ", interactiveSafe(label), interactiveSafe(fallback))
	}
	value, err := readInteractiveLine(a.In)
	if err != nil {
		return "", err
	}
	if value == "" {
		value = fallback
	}
	return value, nil
}

func (a *App) interactiveChoose(title string, items []interactiveMenuItem) (string, error) {
	for {
		_, _ = fmt.Fprintf(a.Err, "\n%s\n", interactiveSafe(title))
		for _, item := range items {
			_, _ = fmt.Fprintf(a.Err, "  %s. %s\n", interactiveSafe(item.Key), interactiveSafe(item.Label))
		}
		value, err := a.interactivePrompt("選択", "")
		if err != nil {
			return "", err
		}
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "q" || value == "quit" || value == "back" {
			value = "0"
		}
		for _, item := range items {
			if value == strings.ToLower(item.Key) {
				return item.Key, nil
			}
		}
		_, _ = fmt.Fprintln(a.Err, "一覧にある番号を入力してください。")
	}
}

func (a *App) interactiveRequired(label, fallback string) (string, error) {
	for {
		value, err := a.interactivePrompt(label, fallback)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value), nil
		}
		_, _ = fmt.Fprintln(a.Err, "空にはできません。")
	}
}

func (a *App) interactiveAbsolutePath(label, fallback string) (string, error) {
	for {
		value, err := a.interactiveRequired(label, fallback)
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(value) && filepath.Clean(value) == value && value != "/" && strings.IndexFunc(value, unicode.IsControl) == -1 {
			return value, nil
		}
		_, _ = fmt.Fprintln(a.Err, "正規化済みの絶対パスを入力してください。")
	}
}

func (a *App) interactiveName(label, fallback string) (string, error) {
	for {
		value, err := a.interactiveRequired(label, fallback)
		if err != nil {
			return "", err
		}
		if core.ValidName(value) {
			return value, nil
		}
		_, _ = fmt.Fprintln(a.Err, "英小文字で始まり、英小文字・数字・ハイフンだけの名前を入力してください。")
	}
}

func (a *App) interactiveID(label string) (string, error) {
	for {
		value, err := a.interactiveRequired(label, "")
		if err != nil {
			return "", err
		}
		if core.ValidID(value) {
			return value, nil
		}
		_, _ = fmt.Fprintln(a.Err, "32桁の小文字16進数IDを入力してください。")
	}
}

func (a *App) interactiveDuration(label string, fallback time.Duration, minimum, maximum time.Duration) (time.Duration, error) {
	for {
		value, err := a.interactiveRequired(label, fallback.String())
		if err != nil {
			return 0, err
		}
		duration, err := time.ParseDuration(value)
		if err == nil && duration >= minimum && duration <= maximum {
			return duration, nil
		}
		_, _ = fmt.Fprintf(a.Err, "%s〜%sのGo duration形式で入力してください。\n", minimum, maximum)
	}
}

func (a *App) interactiveURL(label, fallback string) (string, error) {
	for {
		value, err := a.interactiveRequired(label, fallback)
		if err != nil {
			return "", err
		}
		u, err := url.Parse(value)
		if err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == "" {
			return value, nil
		}
		_, _ = fmt.Fprintln(a.Err, "https://host:port 形式のoriginを入力してください。")
	}
}

func (a *App) interactiveDigest(label string) (string, error) {
	for {
		value, err := a.interactiveRequired(label, "")
		if err != nil {
			return "", err
		}
		if interactiveDigestPattern.MatchString(value) {
			return value, nil
		}
		_, _ = fmt.Fprintln(a.Err, "sha256: と64桁の小文字16進数を入力してください。")
	}
}

func (a *App) interactiveBool(label string, fallback bool) (bool, error) {
	defaultValue := "no"
	if fallback {
		defaultValue = "yes"
	}
	for {
		value, err := a.interactivePrompt(label+" (yes/no)", defaultValue)
		if err != nil {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "y", "yes", "はい":
			return true, nil
		case "n", "no", "いいえ":
			return false, nil
		default:
			_, _ = fmt.Fprintln(a.Err, "yes または no を入力してください。")
		}
	}
}

func (a *App) interactiveConfirm(summary, phrase string) (bool, error) {
	_, _ = fmt.Fprintf(a.Err, "\n%s\n", interactiveSafe(summary))
	value, err := a.interactivePrompt("続行するには "+phrase+" と正確に入力", "")
	if err != nil {
		return false, err
	}
	if value != phrase {
		_, _ = fmt.Fprintln(a.Err, "確認語が一致しないため、変更しませんでした。")
		return false, nil
	}
	return true, nil
}

func containsStateFlag(args []string) bool {
	for _, arg := range args {
		if arg == "--state" || strings.HasPrefix(arg, "--state=") {
			return true
		}
	}
	return false
}

func (a *App) executeInteractiveCommand(ctx context.Context, args []string, allowPrompts bool) error {
	nested := &App{
		State:          a.State,
		JSON:           false,
		NonInteractive: !allowPrompts,
		In:             a.In,
		Out:            a.Out,
		Err:            a.Err,
	}
	root := nested.Command()
	full := make([]string, 0, len(args)+3)
	if !containsStateFlag(args) {
		full = append(full, "--state", a.State)
	}
	if !allowPrompts {
		full = append(full, "--non-interactive")
	}
	full = append(full, args...)
	root.SetArgs(full)
	return root.ExecuteContext(ctx)
}

func (a *App) interactiveRun(ctx context.Context, args []string, allowPrompts bool) bool {
	if err := a.executeInteractiveCommand(ctx, args, allowPrompts); err != nil {
		_, _ = fmt.Fprintf(a.Err, "\n[未完了] %s\n", interactiveSafe(err.Error()))
		return false
	}
	_, _ = fmt.Fprintln(a.Err, "\n[完了]")
	return true
}

func (a *App) defaultNodeConfig() string {
	return filepath.Join(filepath.Dir(a.State), "node-a", "agent.json")
}

func (a *App) runInteractive(ctx context.Context) error {
	_, _ = fmt.Fprintf(a.Err, "\nRunnerLoom %s 対話モード\n", Version)
	_, _ = fmt.Fprintln(a.Err, "既存のCLIと同じ処理を、確認付きメニューから実行します。")
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		_, _ = fmt.Fprintf(a.Err, "\nController state: %s\n", interactiveSafe(a.State))
		choice, err := a.interactiveChoose("メインメニュー", []interactiveMenuItem{
			{"1", "初期セットアップ"},
			{"2", "状態・診断"},
			{"3", "Image cache"},
			{"4", "VM network"},
			{"5", "Node管理"},
			{"6", "Pool・GitHub"},
			{"7", "Backup・maintenance"},
			{"8", "systemd service"},
			{"9", "Controller stateを変更"},
			{"10", "CLIヘルプ"},
			{"0", "終了"},
		})
		if errors.Is(err, io.EOF) {
			_, _ = fmt.Fprintln(a.Err, "\n入力が終了したため対話モードを閉じます。")
			return nil
		}
		if err != nil {
			return err
		}
		switch choice {
		case "0":
			_, _ = fmt.Fprintln(a.Err, "対話モードを終了します。")
			return nil
		case "1":
			_, _ = fmt.Fprintln(a.Err, "既存の対話式setupを開始します。設定保存後もImage、network、service、GitHub Jobの確認は別工程です。")
			a.interactiveRun(ctx, []string{"setup"}, true)
		case "2":
			if err = a.runInteractiveStatus(ctx); err != nil && !errors.Is(err, io.EOF) {
				return err
			}
		case "3":
			if err = a.runInteractiveCache(ctx); err != nil && !errors.Is(err, io.EOF) {
				return err
			}
		case "4":
			if err = a.runInteractiveNetwork(ctx); err != nil && !errors.Is(err, io.EOF) {
				return err
			}
		case "5":
			if err = a.runInteractiveNodes(ctx); err != nil && !errors.Is(err, io.EOF) {
				return err
			}
		case "6":
			if err = a.runInteractivePools(ctx); err != nil && !errors.Is(err, io.EOF) {
				return err
			}
		case "7":
			if err = a.runInteractiveMaintenance(ctx); err != nil && !errors.Is(err, io.EOF) {
				return err
			}
		case "8":
			if err = a.runInteractiveServices(ctx); err != nil && !errors.Is(err, io.EOF) {
				return err
			}
		case "9":
			state, promptErr := a.interactiveAbsolutePath("Controller state", a.State)
			if promptErr != nil {
				return promptErr
			}
			a.State = state
		case "10":
			a.interactiveRun(ctx, []string{"--help"}, false)
		}
	}
}

func (a *App) runInteractiveStatus(ctx context.Context) error {
	for {
		choice, err := a.interactiveChoose("状態・診断", []interactiveMenuItem{
			{"1", "Host診断 (doctor)"},
			{"2", "全体状態"},
			{"3", "Node一覧"},
			{"4", "Pool一覧"},
			{"5", "Poolの配置可否"},
			{"6", "GitHub接続確認"},
			{"0", "戻る"},
		})
		if err != nil {
			return err
		}
		switch choice {
		case "0":
			return nil
		case "1":
			a.interactiveRun(ctx, []string{"doctor"}, false)
		case "2":
			a.interactiveRun(ctx, []string{"status"}, false)
		case "3":
			a.interactiveRun(ctx, []string{"node", "list"}, false)
		case "4":
			a.interactiveRun(ctx, []string{"pool", "list"}, false)
		case "5":
			name, promptErr := a.interactiveName("Pool名", "linux-lite")
			if promptErr != nil {
				return promptErr
			}
			a.interactiveRun(ctx, []string{"pool", "explain", name}, false)
		case "6":
			a.interactiveRun(ctx, []string{"github", "check"}, false)
		}
	}
}

func (a *App) interactiveCacheTarget() ([]string, bool, error) {
	choice, err := a.interactiveChoose("対象cache", []interactiveMenuItem{
		{"1", "Controller cache"},
		{"2", "Node cache"},
		{"0", "戻る"},
	})
	if err != nil {
		return nil, false, err
	}
	switch choice {
	case "0":
		return nil, false, nil
	case "1":
		value, err := a.interactiveRequired("Controller cache上限 GiB", "100")
		if err != nil {
			return nil, false, err
		}
		limit, err := strconv.ParseInt(value, 10, 64)
		if err != nil || limit < 1 {
			return nil, false, errors.New("cache上限は1以上の整数GiBで指定してください")
		}
		return []string{"--cache-gib", strconv.FormatInt(limit, 10)}, true, nil
	case "2":
		path, err := a.interactiveAbsolutePath("Node agent.json", a.defaultNodeConfig())
		if err != nil {
			return nil, false, err
		}
		return []string{"--config", path}, true, nil
	default:
		return nil, false, nil
	}
}

func (a *App) runInteractiveCache(ctx context.Context) error {
	for {
		choice, err := a.interactiveChoose("Image cache", []interactiveMenuItem{
			{"1", "使用量と参照状態を見る"},
			{"2", "SHA-256とqcow2構造まで完全検証"},
			{"3", "整理候補を確認 (dry-run)"},
			{"4", "整理候補を再検査して削除"},
			{"5", "同一HostのControllerからNodeへseed"},
			{"0", "戻る"},
		})
		if err != nil {
			return err
		}
		if choice == "0" {
			return nil
		}
		if choice == "5" {
			configPath, promptErr := a.interactiveAbsolutePath("Node agent.json", a.defaultNodeConfig())
			if promptErr != nil {
				return promptErr
			}
			sourceState, promptErr := a.interactiveAbsolutePath("Controller state", a.State)
			if promptErr != nil {
				return promptErr
			}
			digest, promptErr := a.interactiveDigest("seedするImage digest")
			if promptErr != nil {
				return promptErr
			}
			ok, promptErr := a.interactiveConfirm("Agentを停止した状態で、検証済みImageをhard linkします。cross-filesystemの場合は拒否されます。", "SEED")
			if promptErr != nil {
				return promptErr
			}
			if ok {
				a.interactiveRun(ctx, []string{"cache", "seed", digest, "--config", configPath, "--source-state", sourceState}, false)
			}
			continue
		}
		target, selected, promptErr := a.interactiveCacheTarget()
		if promptErr != nil {
			return promptErr
		}
		if !selected {
			continue
		}
		switch choice {
		case "1":
			args := append([]string{"cache", "status"}, target...)
			a.interactiveRun(ctx, args, false)
		case "2":
			args := append([]string{"cache", "status", "--verify"}, target...)
			a.interactiveRun(ctx, args, false)
		case "3", "4":
			older, promptErr := a.interactiveDuration("未参照期間", 7*24*time.Hour, 24*time.Hour, 3650*24*time.Hour)
			if promptErr != nil {
				return promptErr
			}
			args := append([]string{"cache", "prune", "--older-than", older.String()}, target...)
			if !a.interactiveRun(ctx, args, false) || choice == "3" {
				continue
			}
			ok, promptErr := a.interactiveConfirm("上のdry-run候補を再検査して削除します。対象ControllerまたはAgent serviceは停止している必要があります。", "PRUNE")
			if promptErr != nil {
				return promptErr
			}
			if ok {
				a.interactiveRun(ctx, append(args, "--apply"), false)
			}
		}
	}
}

func (a *App) runInteractiveNetwork(ctx context.Context) error {
	for {
		choice, err := a.interactiveChoose("VM network", []interactiveMenuItem{
			{"1", "変更計画を見る"},
			{"2", "現在の隔離状態を検査"},
			{"3", "計画を確認して適用"},
			{"0", "戻る"},
		})
		if err != nil {
			return err
		}
		if choice == "0" {
			return nil
		}
		configPath, promptErr := a.interactiveAbsolutePath("Node agent.json", a.defaultNodeConfig())
		if promptErr != nil {
			return promptErr
		}
		switch choice {
		case "1":
			a.interactiveRun(ctx, []string{"network", "plan", "--config", configPath}, false)
		case "2":
			a.interactiveRun(ctx, []string{"network", "check", "--config", configPath}, false)
		case "3":
			if !a.interactiveRun(ctx, []string{"network", "plan", "--config", configPath}, false) {
				continue
			}
			ok, promptErr := a.interactiveConfirm("上のRunnerLoom専用networkとfirewall規則だけを適用します。稼働中VMがある場合は拒否されます。", "APPLY")
			if promptErr != nil {
				return promptErr
			}
			if ok && a.interactiveRun(ctx, []string{"network", "apply", "--config", configPath}, false) {
				a.interactiveRun(ctx, []string{"network", "check", "--config", configPath}, false)
			}
		}
	}
}

func (a *App) runInteractiveNodes(ctx context.Context) error {
	for {
		choice, err := a.interactiveChoose("Node管理", []interactiveMenuItem{
			{"1", "Node一覧"},
			{"2", "承認待ち申請"},
			{"3", "期限付き招待を作成"},
			{"4", "参加申請を承認"},
			{"5", "Nodeから参加申請・証明書受領"},
			{"6", "新規受付を停止 (drain)"},
			{"7", "新規受付を再開 (resume)"},
			{"8", "Node証明書を失効"},
			{"0", "戻る"},
		})
		if err != nil {
			return err
		}
		switch choice {
		case "0":
			return nil
		case "1":
			a.interactiveRun(ctx, []string{"node", "list"}, false)
		case "2":
			a.interactiveRun(ctx, []string{"node", "pending"}, false)
		case "3":
			endpoint, promptErr := a.interactiveURL("Nodeから到達できるController URL", "https://127.0.0.1:8443")
			if promptErr != nil {
				return promptErr
			}
			defaultOut := filepath.Join(a.State, "invitations", "node-"+time.Now().UTC().Format("20060102T150405Z")+".json")
			out, promptErr := a.interactiveAbsolutePath("招待の新規保存先", defaultOut)
			if promptErr != nil {
				return promptErr
			}
			ttl, promptErr := a.interactiveDuration("有効期間", 10*time.Minute, time.Minute, time.Hour)
			if promptErr != nil {
				return promptErr
			}
			ok, promptErr := a.interactiveConfirm("一回限りの秘密を0600ファイルへ保存します。画面には秘密本文を表示しません。", "CREATE")
			if promptErr != nil {
				return promptErr
			}
			if ok {
				a.interactiveRun(ctx, []string{"node", "invite", "--url", endpoint, "--out", out, "--ttl", ttl.String()}, false)
			}
		case "4":
			a.interactiveRun(ctx, []string{"node", "pending"}, false)
			id, promptErr := a.interactiveID("承認する申請ID")
			if promptErr != nil {
				return promptErr
			}
			ok, promptErr := a.interactiveConfirm("表示されたNode名とCSR指紋を別経路でも照合してから承認してください。", "APPROVE "+id)
			if promptErr != nil {
				return promptErr
			}
			if ok {
				a.interactiveRun(ctx, []string{"node", "approve", id}, false)
			}
		case "5":
			configPath, promptErr := a.interactiveAbsolutePath("Node agent.json", a.defaultNodeConfig())
			if promptErr != nil {
				return promptErr
			}
			invitation, promptErr := a.interactiveAbsolutePath("招待ファイル", filepath.Join(filepath.Dir(configPath), "invitation.json"))
			if promptErr != nil {
				return promptErr
			}
			ok, promptErr := a.interactiveConfirm("参加申請を送信します。PendingApproval後はControllerで承認し、同じ操作を再実行して証明書を受け取ります。", "JOIN")
			if promptErr != nil {
				return promptErr
			}
			if ok {
				a.interactiveRun(ctx, []string{"node", "join", "--config", configPath, "--invitation", invitation}, false)
			}
		case "6", "7", "8":
			a.interactiveRun(ctx, []string{"node", "list"}, false)
			name, promptErr := a.interactiveName("Node名", "node-a")
			if promptErr != nil {
				return promptErr
			}
			action := "drain"
			phrase := "DRAIN " + name
			summary := "新しいJobの受付だけを停止します。実行中Jobは中断しません。"
			if choice == "7" {
				action = "resume"
				phrase = "RESUME " + name
				summary = "このNodeの新しいJob受付を再開します。"
			} else if choice == "8" {
				action = "revoke"
				phrase = "REVOKE " + name
				summary = "Node証明書を失効し、以後の要求を拒否します。既存VMを停止済みとは扱いません。"
			}
			ok, promptErr := a.interactiveConfirm(summary, phrase)
			if promptErr != nil {
				return promptErr
			}
			if ok {
				a.interactiveRun(ctx, []string{"node", action, name}, false)
			}
		}
	}
}

func (a *App) runInteractivePools(ctx context.Context) error {
	for {
		choice, err := a.interactiveChoose("Pool・GitHub", []interactiveMenuItem{
			{"1", "Pool一覧"},
			{"2", "Poolの配置可否"},
			{"3", "GitHub App・Runner Group・Repository権限を検査"},
			{"0", "戻る"},
		})
		if err != nil {
			return err
		}
		switch choice {
		case "0":
			return nil
		case "1":
			a.interactiveRun(ctx, []string{"pool", "list"}, false)
		case "2":
			name, promptErr := a.interactiveName("Pool名", "linux-lite")
			if promptErr != nil {
				return promptErr
			}
			a.interactiveRun(ctx, []string{"pool", "explain", name}, false)
		case "3":
			a.interactiveRun(ctx, []string{"github", "check"}, false)
		}
	}
}

func (a *App) runInteractiveMaintenance(ctx context.Context) error {
	for {
		choice, err := a.interactiveChoose("Backup・maintenance", []interactiveMenuItem{
			{"1", "Controller DBの整合したbackupを作成"},
			{"2", "DB整理候補を確認 (dry-run)"},
			{"3", "DB整理を適用"},
			{"0", "戻る"},
		})
		if err != nil {
			return err
		}
		switch choice {
		case "0":
			return nil
		case "1":
			defaultOut := "/var/backups/runnerloom/controller-" + time.Now().UTC().Format("20060102T150405Z") + ".db"
			out, promptErr := a.interactiveAbsolutePath("新規backupファイル", defaultOut)
			if promptErr != nil {
				return promptErr
			}
			ok, promptErr := a.interactiveConfirm("SQLiteの整合したsnapshotを新規ファイルへ保存します。復旧にはmaster key・CA・binding・GitHub認証も別途必要です。", "BACKUP")
			if promptErr != nil {
				return promptErr
			}
			if ok {
				a.interactiveRun(ctx, []string{"backup", "--out", out}, false)
			}
		case "2":
			a.interactiveRun(ctx, []string{"maintenance", "compact"}, false)
		case "3":
			if !a.interactiveRun(ctx, []string{"maintenance", "compact"}, false) {
				continue
			}
			ok, promptErr := a.interactiveConfirm("上の候補だけを削除し、WAL checkpointとVACUUMを行います。Controller serviceを停止しておく必要があります。", "COMPACT")
			if promptErr != nil {
				return promptErr
			}
			if ok {
				a.interactiveRun(ctx, []string{"maintenance", "compact", "--apply"}, false)
			}
		}
	}
}

func executableFallback() string {
	path, err := os.Executable()
	if err == nil && filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return "/usr/local/bin/runnerloom"
}

func (a *App) runInteractiveServices(ctx context.Context) error {
	for {
		choice, err := a.interactiveChoose("systemd service", []interactiveMenuItem{
			{"1", "Controller serviceをinstall/update"},
			{"2", "Agent serviceをinstall/update"},
			{"0", "戻る"},
		})
		if err != nil {
			return err
		}
		if choice == "0" {
			return nil
		}
		binary, promptErr := a.interactiveAbsolutePath("RunnerLoom binary", executableFallback())
		if promptErr != nil {
			return promptErr
		}
		if choice == "1" {
			state, promptErr := a.interactiveAbsolutePath("Controller state", a.State)
			if promptErr != nil {
				return promptErr
			}
			listen, promptErr := a.interactiveRequired("待受address", "127.0.0.1:8443")
			if promptErr != nil {
				return promptErr
			}
			advertise, promptErr := a.interactiveURL("Nodeから見えるController URL", "https://127.0.0.1:8443")
			if promptErr != nil {
				return promptErr
			}
			discoverable, promptErr := a.interactiveBool("LAN自動発見候補として公開", false)
			if promptErr != nil {
				return promptErr
			}
			start, promptErr := a.interactiveBool("install後にenable --now", true)
			if promptErr != nil {
				return promptErr
			}
			ok, promptErr := a.interactiveConfirm("RunnerLoom所有のController unitを生成します。既存の他者所有unitは上書きしません。", "INSTALL")
			if promptErr != nil {
				return promptErr
			}
			if ok {
				args := []string{"--state", state, "service", "install", "--role", "controller", "--binary", binary, "--listen", listen, "--advertise", advertise}
				if discoverable {
					args = append(args, "--discoverable")
				}
				if start {
					args = append(args, "--start")
				}
				a.interactiveRun(ctx, args, false)
			}
			continue
		}
		configPath, promptErr := a.interactiveAbsolutePath("Node agent.json", a.defaultNodeConfig())
		if promptErr != nil {
			return promptErr
		}
		nodeState, promptErr := a.interactiveAbsolutePath("Node state", filepath.Dir(configPath))
		if promptErr != nil {
			return promptErr
		}
		start, promptErr := a.interactiveBool("install後にenable --now", true)
		if promptErr != nil {
			return promptErr
		}
		ok, promptErr := a.interactiveConfirm("Agentはlibvirtとfirewallを操作する信頼済みhost processです。先にnetwork apply/checkを完了してください。", "INSTALL")
		if promptErr != nil {
			return promptErr
		}
		if ok {
			args := []string{"--state", nodeState, "service", "install", "--role", "agent", "--config", configPath, "--binary", binary}
			if start {
				args = append(args, "--start")
			}
			a.interactiveRun(ctx, args, false)
		}
	}
}
