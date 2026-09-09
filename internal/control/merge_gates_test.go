package control_test

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MOVEI144/RunnerLoom/internal/control"
	"github.com/MOVEI144/RunnerLoom/internal/core"
)

type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadline time.Time
	failure  error
}

func (w *deadlineRecorder) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	return w.failure
}

func TestAuthorizedImagesUseSharedTransferDeadline(t *testing.T) {
	l := newLab(t)
	a, _ := l.node(t, "node-a", l.c.Nodes[0].Budget)
	transport := a.Client.HTTP.Transport.(*http.Transport)
	leaf, err := x509.ParseCertificate(transport.TLSClientConfig.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	handler := control.New(l.s, l.ca, l.c.Name).Handler()
	for _, kind := range []string{"authorized", "unauthenticated", "deadline-error"} {
		t.Run(kind, func(t *testing.T) {
			req := httptest.NewRequest("GET", "https://controller/v1/images/"+strings.TrimPrefix(l.c.Images[0].Digest, "sha256:"), nil)
			if kind != "unauthenticated" {
				req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}, VerifiedChains: [][]*x509.Certificate{{leaf}}}
			}
			w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
			if kind == "deadline-error" {
				w.failure = errors.New("cannot set deadline")
			}
			before := time.Now()
			handler.ServeHTTP(w, req)
			switch kind {
			case "authorized":
				if w.Code != 200 || w.Body.String() != string(l.image) {
					t.Fatal(w.Code, w.Body.String())
				}
				budget := w.deadline.Sub(before)
				if budget < core.ImageTransferTimeout || budget > core.ImageTransferTimeout+5*time.Second {
					t.Fatal("image stream uses a mismatched deadline", budget)
				}
			case "unauthenticated":
				if w.Code != 403 || !w.deadline.IsZero() {
					t.Fatal("unauthenticated stream received a long deadline")
				}
			case "deadline-error":
				if w.Code != 500 || strings.Contains(w.Body.String(), string(l.image)) {
					t.Fatal("deadline failure was ignored")
				}
			}
		})
	}
}
