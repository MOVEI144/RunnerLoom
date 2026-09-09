package cli

import (
 "context"
 "errors"
 "fmt"
 "os"
 "os/exec"
 "os/user"
 "path/filepath"
 "strconv"
 "strings"
 "time"

 "github.com/spf13/cobra"
 "github.com/MOVEI144/RunnerLoom/internal/agent"
 "github.com/MOVEI144/RunnerLoom/internal/core"
)

func systemdArg(s string)(string,error){if strings.ContainsAny(s,"\x00\r\n%")||s==""{return "",errors.New("unsafe systemd argument")};return `"`+strings.ReplaceAll(strings.ReplaceAll(s,`\`,`\\`),`"`,`\"`)+`"`,nil}
func systemdExec(args ...string)(string,error){out:=[]string{};for _,arg:=range args{s,e:=systemdArg(arg);if e!=nil{return "",e};out=append(out,s)};return strings.Join(out," "),nil}
func runSystem(ctx context.Context,program string,args ...string)error{ctx,cancel:=context.WithTimeout(ctx,2*time.Minute);defer cancel();cmd:=exec.CommandContext(ctx,program,args...);cmd.Env=[]string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin","LC_ALL=C"};cmd.Stdout=os.Stderr;cmd.Stderr=os.Stderr;return cmd.Run()}
func chownPrivateTree(root string,uid,gid int)error{return filepath.WalkDir(root,func(path string,d os.DirEntry,e error)error{if e!=nil{return e};if d.Type()&os.ModeSymlink!=0{return errors.New("symlink inside controller state; refusing ownership changes")};if !d.IsDir()&& !d.Type().IsRegular(){return errors.New("non-regular file in controller state")};return os.Chown(path,uid,gid)})}
func ServiceUnit(binary,role,state,config,listen,advertise string)(name,unit string,err error){
 if !filepath.IsAbs(binary)||!filepath.IsAbs(state){return "","",errors.New("absolute binary and state paths required")}
 var command string;var writable []string;account:="runnerloom-controller"
 switch role{
 case "controller":name="runnerloom-controller.service";command,err=systemdExec(binary,"controller","run","--state",state,"--listen",listen,"--advertise",advertise);writable=[]string{state}
 case "agent":c,e:=agent.LoadConfig(config);if e!=nil{return "","",e};name="runnerloom-agent-"+c.Node+".service";account="root";command,err=systemdExec(binary,"agent","run","--config",config,"--restore-network");writable=[]string{c.StateDir,c.DiskDir}
 default:return "","",errors.New("role must be controller or agent")
 };if err!=nil{return "","",err};rw,err:=systemdExec(writable...);if err!=nil{return "","",err}
 caps:="CapabilityBoundingSet=\nPrivateDevices=yes\nRestrictAddressFamilies=AF_UNIX AF_INET AF_INET6\n";if role=="agent"{caps="CapabilityBoundingSet=CAP_NET_ADMIN CAP_DAC_OVERRIDE CAP_DAC_READ_SEARCH CAP_CHOWN CAP_FOWNER CAP_SETUID CAP_SETGID\nRestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK\n"}
 unit=fmt.Sprintf(`# Managed by RunnerLoom. Only this unit is modified by the installer.
[Unit]
Description=RunnerLoom %s
Wants=network-online.target
After=network-online.target libvirtd.service
[Service]
Type=simple
User=%s
ExecStart=%s
Restart=on-failure
RestartSec=5
TimeoutStopSec=45
KillMode=mixed
UMask=0077
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=strict
ProtectHome=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictSUIDSGID=yes
LockPersonality=yes
ReadWritePaths=%s
%s[Install]
WantedBy=multi-user.target
`,role,account,command,rw,caps);return name,unit,nil
}
func (a *App) addService(root *cobra.Command){
 group:=&cobra.Command{Use:"service",Short:"生成したsystemdユニットを確認・明示インストール"};root.AddCommand(group)
 for _,action:=range []string{"render","install"}{operation:=action;var binary,role,config,listen,advertise string;var start bool
  cmd:=add(group,operation,"Controllerは専用ユーザー、Agentは信頼されたホスト管理プロセスとして動作",0,func(c *cobra.Command,_ []string)error{
   if binary==""{var e error;binary,e=os.Executable();if e!=nil{return e}}
   name,unit,e:=ServiceUnit(binary,role,a.State,config,listen,advertise);if e!=nil{return e};if operation=="render"{if a.JSON{return a.output(map[string]string{"name":name,"unit":unit})};_,e=fmt.Fprint(a.Out,unit);return e}
   if os.Geteuid()!=0{return core.Fail("ROOT_REQUIRED","service installは管理者として実行してください",nil)}
   if !strings.HasPrefix(a.State,"/var/lib/"){return errors.New("systemd用の状態保存先は /var/lib/ 以下にしてください")}
   if role=="controller"{
    lock,e:=core.AcquireLock(a.State,"controller");if e!=nil{return e};defer lock.Close()
    u,e:=user.Lookup("runnerloom-controller");if e!=nil{if e=runSystem(c.Context(),"/usr/sbin/useradd","--system","--home-dir",a.State,"--shell","/usr/sbin/nologin","runnerloom-controller");e!=nil{return e};u,e=user.Lookup("runnerloom-controller");if e!=nil{return e}}
    uid,e:=strconv.Atoi(u.Uid);if e!=nil{return e};gid,e:=strconv.Atoi(u.Gid);if e!=nil{return e};if uid==0{return errors.New("controller account must not be root")};if e=chownPrivateTree(a.State,uid,gid);e!=nil{return e}
   }else{nc,e:=agent.LoadConfig(config);if e!=nil{return e};if !strings.HasPrefix(nc.StateDir,"/var/lib/")||!strings.HasPrefix(nc.DiskDir,"/var/lib/"){return errors.New("systemd Agentの保存先は /var/lib/ 以下にしてください")};if _,e=core.ReadSecret(filepath.Join(nc.StateDir,"network","network-seal.json"));e!=nil{return errors.New("先にnetwork applyで隔離設定を確認してください")}}
   path:=filepath.Join("/etc/systemd/system",name);if b,e:=os.ReadFile(path);e==nil&&!strings.HasPrefix(string(b),"# Managed by RunnerLoom."){return errors.New("同名の既存ユニットはRunnerLoom所有ではありません")}
   f,e:=os.CreateTemp("/etc/systemd/system",".runnerloom-unit-");if e!=nil{return e};defer os.Remove(f.Name());if _,e=f.WriteString(unit);e==nil{e=f.Chmod(0644)};if e==nil{e=f.Sync()};f.Close();if e!=nil{return e};if e=os.Rename(f.Name(),path);e!=nil{return e};if e=runSystem(c.Context(),"/usr/bin/systemctl","daemon-reload");e!=nil{return e};if start{if e=runSystem(c.Context(),"/usr/bin/systemctl","enable","--now",name);e!=nil{return e}};return a.output(map[string]any{"unit":path,"started":start})
  });cmd.Flags().StringVar(&binary,"binary","","インストール済みRunnerLoomバイナリの絶対パス");cmd.Flags().StringVar(&role,"role","controller","controller / agent");cmd.Flags().StringVar(&config,"config","","Agentの場合は設定JSON");cmd.Flags().StringVar(&listen,"listen","127.0.0.1:8443","Controllerの明示待受");cmd.Flags().StringVar(&advertise,"advertise","https://127.0.0.1:8443","Nodeから見えるURL");cmd.Flags().BoolVar(&start,"start",false,"install後にサービスを有効化して起動する")
 }
}
