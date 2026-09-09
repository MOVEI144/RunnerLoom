// Package control exposes only the node protocol. Administrative changes remain
// local CLI operations; a node certificate cannot approve peers or change pools.
package control

import (
 "context"
 "crypto/tls"
 "crypto/x509"
 "encoding/json"
 "errors"
 "fmt"
 "net"
 "net/http"
 "net/url"
 "os"
 "path/filepath"
 "strings"
 "sync"
 "time"

 "github.com/MOVEI144/RunnerLoom/internal/core"
)

type Server struct { Store *core.Store; CA core.CA; Cluster string; mu sync.Mutex; attempts map[string]window }
type window struct { Start time.Time; Count int }
func New(s *core.Store,ca core.CA,cluster string)*Server{return &Server{Store:s,CA:ca,Cluster:cluster,attempts:map[string]window{}}}
func respond(w http.ResponseWriter,status int,v any){w.Header().Set("Content-Type","application/json");w.Header().Set("Cache-Control","no-store");w.WriteHeader(status);_ = json.NewEncoder(w).Encode(v)}
func failure(w http.ResponseWriter,status int,err error){var e *core.Error;if !errors.As(err,&e){e=&core.Error{Code:"REQUEST_FAILED",Message:"処理できませんでした。Controllerの診断を確認してください"}};respond(w,status,map[string]any{"error":e})}
func decode(w http.ResponseWriter,r *http.Request,v any)bool{r.Body=http.MaxBytesReader(w,r.Body,core.MaxJSON);if e:=core.Decode(r.Body,v);e!=nil{failure(w,400,core.Fail("INVALID_REQUEST","不正なJSON要求です",nil));return false};return true}
func (s *Server) node(r *http.Request)(string,error){if r.TLS==nil||len(r.TLS.VerifiedChains)==0||len(r.TLS.PeerCertificates)==0{return "",core.Fail("TLS_REQUIRED","Nodeの相互認証が必要です",nil)};n,e:=core.NodeIdentity(r.TLS.PeerCertificates[0],s.Cluster);if e!=nil{return "",e};return n,s.Store.Authorize(r.Context(),n)}
func (s *Server) allowEnrollment(ip string)bool{s.mu.Lock();defer s.mu.Unlock();now:=time.Now();if len(s.attempts)>1024{for k,v:=range s.attempts{if now.Sub(v.Start)>time.Minute{delete(s.attempts,k)}};if len(s.attempts)>1024{return false}};v:=s.attempts[ip];if now.Sub(v.Start)>time.Minute{v=window{Start:now}};v.Count++;s.attempts[ip]=v;return v.Count<=30}
func (s *Server) Handler()http.Handler{
 mux:=http.NewServeMux()
 mux.HandleFunc("POST /v1/enroll",func(w http.ResponseWriter,r *http.Request){ip,_,_:=net.SplitHostPort(r.RemoteAddr);if !s.allowEnrollment(ip){failure(w,429,core.Fail("RATE_LIMIT","参加要求が多すぎます",nil));return};var q core.JoinRequest;if !decode(w,r,&q){return};v,e:=s.Store.Join(r.Context(),q);if e!=nil{failure(w,403,e);return};respond(w,200,v)})
 mux.HandleFunc("POST /v1/sync",func(w http.ResponseWriter,r *http.Request){name,e:=s.node(r);if e!=nil{failure(w,403,e);return};var o core.Observation;if !decode(w,r,&o){return};v,e:=s.Store.Sync(r.Context(),name,o);if e!=nil{failure(w,409,e);return};respond(w,200,v)})
 mux.HandleFunc("POST /v1/renew",func(w http.ResponseWriter,r *http.Request){name,e:=s.node(r);if e!=nil{failure(w,403,e);return};var q struct{CSR []byte `json:"csr"`};if !decode(w,r,&q){return};csr,e:=core.ParseCSR(q.CSR);if e!=nil{failure(w,400,e);return};pub,_:=x509.MarshalPKIXPublicKey(csr.PublicKey);old,_:=x509.MarshalPKIXPublicKey(r.TLS.PeerCertificates[0].PublicKey);if core.Hash(pub)!=core.Hash(old){failure(w,403,core.Fail("KEY_MISMATCH","自動更新では既存のNode鍵を使用してください",nil));return};cert,e:=s.CA.Sign(q.CSR,s.Cluster,name);if e!=nil{failure(w,500,e);return};respond(w,200,map[string]any{"certificate":cert})})
 mux.HandleFunc("GET /v1/images/{digest}",func(w http.ResponseWriter,r *http.Request){name,e:=s.node(r);if e!=nil{failure(w,403,e);return};digest:=r.PathValue("digest");if len(digest)!=64||strings.Trim(digest,"0123456789abcdef")!=""{http.NotFound(w,r);return};c,_,e:=s.Store.Config(r.Context());if e!=nil{failure(w,500,e);return};node,ok:=c.Node(name);allowed:=false;if ok{for _,p:=range c.Pools{im,_:=c.Image(p.Image);if c.Eligible(p,node)&&im.Digest=="sha256:"+digest{allowed=true}}};if !allowed{failure(w,403,core.Fail("IMAGE_NOT_ALLOWED","このNodeへ提供しないイメージです",nil));return};path:=filepath.Join(s.Store.Dir,"images",digest+".qcow2");f,e:=os.Open(path);if e!=nil{http.NotFound(w,r);return};defer f.Close();st,e:=f.Stat();if e!=nil||!st.Mode().IsRegular(){http.NotFound(w,r);return};w.Header().Set("Content-Type","application/octet-stream");w.Header().Set("ETag","\"sha256:"+digest+"\"");http.ServeContent(w,r,st.Name(),st.ModTime(),f)})
 return http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){if r.URL.RawQuery!=""{failure(w,400,core.Fail("QUERY_FORBIDDEN","クエリ文字列は使用できません",nil));return};mux.ServeHTTP(w,r)})
}
func (s *Server) TLSConfig(names []string)(*tls.Config,error){
 roots:=x509.NewCertPool();roots.AddCert(s.CA.Certificate);cert,e:=s.CA.ServerCertificate(names);if e!=nil{return nil,e};var mu sync.Mutex;renewAt:=time.Now().Add(24*time.Hour)
 return &tls.Config{MinVersion:tls.VersionTLS13,ClientAuth:tls.VerifyClientCertIfGiven,ClientCAs:roots,GetCertificate:func(*tls.ClientHelloInfo)(*tls.Certificate,error){mu.Lock();defer mu.Unlock();if time.Now().After(renewAt){fresh,e:=s.CA.ServerCertificate(names);if e!=nil{return nil,e};cert=fresh;renewAt=time.Now().Add(24*time.Hour)};return &cert,nil}},nil
}
func (s *Server) Serve(ctx context.Context,listen,advertise string)error{
 u,e:=url.Parse(advertise);if e!=nil||u.Scheme!="https"||u.Host==""||u.Path!=""||u.User!=nil||u.RawQuery!=""||u.Fragment!=""{return fmt.Errorf("advertise must be an https origin")}
 host,port,e:=net.SplitHostPort(listen);if e!=nil||host==""||port=="0"{return errors.New("listen must explicitly specify host and nonzero port")}
 names:=[]string{u.Hostname()};if host!="0.0.0.0"&&host!="::"{names=append(names,host)};cfg,e:=s.TLSConfig(names);if e!=nil{return e}
 ln,e:=net.Listen("tcp",listen);if e!=nil{return e};defer ln.Close()
 server:=&http.Server{Handler:s.Handler(),TLSConfig:cfg,ReadHeaderTimeout:5*time.Second,ReadTimeout:30*time.Second,WriteTimeout:5*time.Minute,IdleTimeout:30*time.Second,MaxHeaderBytes:16384}
 done:=make(chan struct{});defer close(done);go func(){select{case <-ctx.Done():shutdown,cancel:=context.WithTimeout(context.Background(),10*time.Second);defer cancel();_ = server.Shutdown(shutdown);case <-done:}}()
 e=server.Serve(tls.NewListener(ln,cfg));if errors.Is(e,http.ErrServerClosed){return nil};return e
}
