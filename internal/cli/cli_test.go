package cli

import (
 "bytes"
 "context"
 "encoding/json"
 "os"
 "path/filepath"
 "strings"
 "testing"

 "github.com/MOVEI144/RunnerLoom/internal/core"
)
func invoke(args ...string)(int,string,string){var out,errout bytes.Buffer;code:=Run(context.Background(),args,strings.NewReader(""),&out,&errout);return code,out.String(),errout.String()}
func TestVersionJSON(t *testing.T){code,out,err:=invoke("version","--json");if code!=0||err!=""{t.Fatal(code,out,err)};var v map[string]any;if e:=json.Unmarshal([]byte(out),&v);e!=nil||v["ok"]!=true{t.Fatal("invalid machine output",out)}}
func TestHelpIncludesOperations(t *testing.T){code,out,_:=invoke("--help","--json");if code!=0{t.Fatal(code)};for _,want:=range []string{"controller run","agent run","config plan","node approve","network check","smoke-vm"}{if !strings.Contains(out,want){t.Fatal("help does not expose",want)}}}
func TestUnknownFlagFails(t *testing.T){code,_,_:=invoke("version","--not-a-real-flag");if code==0{t.Fatal("unknown flag ignored")}}
func TestNonInteractiveSetupDoesNotPrompt(t *testing.T){code,out,errout:=invoke("setup","--non-interactive","--json","--state",filepath.Join(t.TempDir(),"state"));if code==0||!strings.Contains(out,"MISSING_INPUT"){t.Fatal(code,out,errout)};if strings.Contains(errout,"["){t.Fatal("noninteractive command prompted")}}
func TestSampleRoundTrips(t *testing.T){code,out,errout:=invoke("config","sample");if code!=0{t.Fatal(errout)};var c core.Config;if e:=core.Decode(strings.NewReader(out),&c);e!=nil{t.Fatal(e)};if e:=c.Validate();e!=nil{t.Fatal(e)}}
func configFixture(t *testing.T)string{t.Helper();p:=filepath.Join(t.TempDir(),"cluster.json");b,_:=json.Marshal(core.Example());if e:=os.WriteFile(p,b,0600);e!=nil{t.Fatal(e)};return p}
func TestSetupAndRepeatPreserveIdentity(t *testing.T){file:=configFixture(t);dir:=filepath.Join(t.TempDir(),"controller");args:=[]string{"setup","--file",file,"--apply","--role","controller-node","--non-interactive","--state",dir,"--json"};code,out,errout:=invoke(args...);if code!=0{t.Fatal(code,out,errout)};if !strings.Contains(out,`"networkChanged": false`){t.Fatal("setup falsely claims host changes")};key,e:=os.ReadFile(filepath.Join(dir+"-node","node-key.pem"));if e!=nil{t.Fatal(e)};code,out,errout=invoke(args...);if code!=0{t.Fatal(code,out,errout)};after,e:=os.ReadFile(filepath.Join(dir+"-node","node-key.pem"));if e!=nil||!bytes.Equal(key,after){t.Fatal("setup regenerated an enrolled identity",e)};if strings.Contains(out,"PRIVATE KEY"){t.Fatal("private key leaked in setup output")};code,out,errout=invoke("status","--state",dir,"--json");if code!=0||!strings.Contains(out,`"configured": true`){t.Fatal(code,out,errout)}}
func TestPlanDoesNotApplyConfiguration(t *testing.T){file:=configFixture(t);state:=filepath.Join(t.TempDir(),"controller");code,out,errout:=invoke("config","plan","--file",file,"--state",state,"--json");if code!=0{t.Fatal(code,out,errout)};code,out,errout=invoke("status","--state",state,"--json");if code!=0||!strings.Contains(out,`"configured": false`){t.Fatal("plan silently applied configuration",code,out,errout)}}
func TestInvitationDoesNotPrintSecret(t *testing.T){file:=configFixture(t);dir:=filepath.Join(t.TempDir(),"controller");code,out,errout:=invoke("setup","--file",file,"--apply","--role","controller","--non-interactive","--state",dir,"--json");if code!=0{t.Fatal(out,errout)};path:=filepath.Join(t.TempDir(),"invite.json");code,out,errout=invoke("node","invite","--url","https://controller.example:8443","--out",path,"--state",dir,"--json");if code!=0{t.Fatal(out,errout)};b,e:=os.ReadFile(path);if e!=nil{t.Fatal(e)};var inv core.Invitation;if e=json.Unmarshal(b,&inv);e!=nil{t.Fatal(e)};if inv.Secret==""||strings.Contains(out,inv.Secret)||strings.Contains(errout,inv.Secret){t.Fatal("invitation secret was missing or exposed")}}
func TestServiceUnitSeparatesControllerPrivileges(t *testing.T){name,unit,e:=ServiceUnit("/usr/local/bin/runnerloom","controller","/var/lib/runnerloom/controller","","127.0.0.1:8443","https://controller.local:8443");if e!=nil{t.Fatal(e)};if name!="runnerloom-controller.service"||!strings.Contains(unit,"User=runnerloom-controller")||!strings.Contains(unit,"NoNewPrivileges=yes")||!strings.Contains(unit,"ProtectSystem=strict"){t.Fatal("controller service is not confined")}}
func TestServiceArgumentsDoNotExpand(t *testing.T){for _,bad:=range []string{"hello\nExecStart=/bin/evil","%n","bad\x00value"}{if _,e:=systemdArg(bad);e==nil{t.Fatal("unsafe unit argument accepted")}};quoted,e:=systemdArg(`path with "quotes"`);if e!=nil||!strings.Contains(quoted,`\"`){t.Fatal("argument not quoted safely")}}
