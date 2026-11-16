package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/ebitengine/purego"
)

type pamMessage struct {
	Style int32
	Text  *byte
}
type pamResponse struct {
	Text unsafe.Pointer
	Code int32
}
type pamConversation struct{ Callback, Data uintptr }
type credentials struct{ user, password string }

var pam struct {
	once                  sync.Once
	err                   error
	start                 func(string, string, *pamConversation, *uintptr) int32
	startConf             func(string, string, *pamConversation, string, *uintptr) int32
	authenticate, account func(uintptr, int32) int32
	end                   func(uintptr, int32) int32
	calloc                func(uintptr, uintptr) unsafe.Pointer
	strdup                func(string) unsafe.Pointer
	free                  func(unsafe.Pointer)
	callback              uintptr
	seq                   atomic.Uint64
	requests              sync.Map
}

func loadPAM() {
	h, err := purego.Dlopen("libpam.so.0", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		pam.err = err
		return
	}
	for name, fn := range map[string]any{"pam_start": &pam.start, "pam_authenticate": &pam.authenticate, "pam_acct_mgmt": &pam.account, "pam_end": &pam.end} {
		symbol, e := purego.Dlsym(h, name)
		if e != nil {
			pam.err = e
			return
		}
		purego.RegisterFunc(fn, symbol)
	}
	if symbol, e := purego.Dlsym(h, "pam_start_confdir"); e == nil {
		purego.RegisterFunc(&pam.startConf, symbol)
	}
	libc, e := purego.Dlopen("libc.so.6", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if e != nil {
		pam.err = e
		return
	}
	for name, fn := range map[string]any{"calloc": &pam.calloc, "strdup": &pam.strdup, "free": &pam.free} {
		symbol, e := purego.Dlsym(libc, name)
		if e != nil {
			pam.err = e
			return
		}
		purego.RegisterFunc(fn, symbol)
	}
	pam.callback = purego.NewCallback(converse)
}
func converse(count int32, messages **pamMessage, response **pamResponse, id uintptr) int32 {
	if count < 1 || count > 32 || messages == nil || response == nil {
		return 19
	}
	state, ok := pam.requests.Load(id)
	if !ok {
		return 19
	}
	c := state.(credentials)
	allocation := pam.calloc(uintptr(count), unsafe.Sizeof(pamResponse{}))
	if allocation == nil {
		return 5
	}
	replies := unsafe.Slice((*pamResponse)(allocation), int(count))
	valid := false
	defer func() {
		if !valid {
			for _, r := range replies {
				if r.Text != nil {
					pam.free(r.Text)
				}
			}
			pam.free(allocation)
		}
	}()
	for i, m := range unsafe.Slice(messages, int(count)) {
		if m == nil {
			return 19
		}
		switch m.Style {
		case 1:
			replies[i].Text = pam.strdup(c.password)
			if replies[i].Text == nil {
				return 5
			}
		case 2:
			replies[i].Text = pam.strdup(c.user)
			if replies[i].Text == nil {
				return 5
			}
		case 3, 4:
		default:
			return 19
		}
	}
	*response = (*pamResponse)(allocation)
	valid = true
	return 0
}

// PAM returns an authenticator using a system PAM service, with account policy
// checked after authentication. Credentials are never placed in command lines.
func PAM(service string) (func(context.Context, string, string) error, error) {
	if service == "" || strings.ContainsAny(service, "/\\\x00") || len(service) > 128 {
		return nil, errors.New("invalid PAM service")
	}
	pam.once.Do(loadPAM)
	if pam.err != nil {
		return nil, pam.err
	}
	return func(ctx context.Context, user, password string) error {
		return authenticate(ctx, service, "", user, password)
	}, nil
}
func authenticate(ctx context.Context, service, directory, user, password string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if user == "" || password == "" || strings.ContainsRune(user, 0) || strings.ContainsRune(password, 0) {
		return errors.New("invalid credentials")
	}
	id := uintptr(pam.seq.Add(1))
	pam.requests.Store(id, credentials{user, password})
	defer pam.requests.Delete(id)
	conversation := pamConversation{pam.callback, id}
	var handle uintptr
	var result int32
	if directory == "" {
		result = pam.start(service, user, &conversation, &handle)
	} else {
		result = pam.startConf(service, user, &conversation, directory, &handle)
	}
	if result != 0 {
		return fmt.Errorf("PAM initialization failed (%d)", result)
	}
	defer func() { pam.end(handle, result) }()
	result = pam.authenticate(handle, 0)
	if result == 0 {
		result = pam.account(handle, 0)
	}
	if result != 0 {
		return errors.New("authentication failed")
	}
	return ctx.Err()
}
