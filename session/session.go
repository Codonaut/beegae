// Copyright 2014 beego Author. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// package session provider
//
// Usage:
// import(
//   "github.com/astaxie/beego/session"
// )
//
//	func init() {
//      globalSessions, _ = session.NewManager("memory", `{"cookieName":"gosessionid", "enableSetCookie,omitempty": true, "gclifetime":3600, "maxLifetime": 3600, "secure": false, "sessionIDHashFunc": "sha1", "sessionIDHashKey": "", "cookieLifeTime": 3600, "providerConfig": ""}`)
//		go globalSessions.GC()
//	}
//
// more docs: http://beego.me/docs/module/session.md
package session

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"appengine"

	"github.com/astaxie/beego/utils"
)

// SessionStore contains all data for one session process with specific id.
type SessionStore interface {
	Set(key, value interface{}) error     //set session value
	Get(key interface{}) interface{}      //get session value
	Delete(key interface{}) error         //delete session value
	SessionID() string                    //back current sessionID
	SessionRelease(w http.ResponseWriter) // release the resource & save data to provider & return the data
	Flush() error                         //delete all data
}

// Provider contains global session methods and saved SessionStores.
// it can operate a SessionStore by its id.
type Provider interface {
	SessionInit(gclifetime int64, config string) error
	SessionRead(sid string, c appengine.Context) (SessionStore, error)
	SessionExist(sid string, c appengine.Context) bool
	SessionRegenerate(oldsid, sid string, c appengine.Context) (SessionStore, error)
	SessionDestroy(sid string, c appengine.Context) error
	SessionAll() int //get all active session
	SessionGC(c appengine.Context)
}

var provides = make(map[string]Provider)

// Register makes a session provide available by the provided name.
// If Register is called twice with the same name or if driver is nil,
// it panics.
func Register(name string, provide Provider) {
	if provide == nil {
		panic("session: Register provide is nil")
	}
	if _, dup := provides[name]; dup {
		panic("session: Register called twice for provider " + name)
	}
	provides[name] = provide
}

type managerConfig struct {
	CookieName        string `json:"cookieName"`
	EnableSetCookie   bool   `json:"enableSetCookie,omitempty"`
	Gclifetime        int64  `json:"gclifetime"`
	Maxlifetime       int64  `json:"maxLifetime"`
	Secure            bool   `json:"secure"`
	SessionIDHashFunc string `json:"sessionIDHashFunc"`
	SessionIDHashKey  string `json:"sessionIDHashKey"`
	CookieLifeTime    int    `json:"cookieLifeTime"`
	ProviderConfig    string `json:"providerConfig"`
	Domain            string `json:"domain"`
}

// Manager contains Provider and its configuration.
type Manager struct {
	provider Provider
	config   *managerConfig
}

// Create new Manager with provider name and json config string.
// provider name:
// 1. cookie
// 2. file
// 3. memory
// 4. redis
// 5. mysql
// json config:
// 1. is https  default false
// 2. hashfunc  default sha1
// 3. hashkey default beegosessionkey
// 4. maxage default is none
func NewManager(provideName, config string) (*Manager, error) {
	provider, ok := provides[provideName]
	if !ok {
		return nil, fmt.Errorf("session: unknown provider %q (forgotten import?)", provideName)
	}
	cf := new(managerConfig)
	cf.EnableSetCookie = true
	err := json.Unmarshal([]byte(config), cf)
	if err != nil {
		return nil, err
	}
	if cf.Maxlifetime == 0 {
		cf.Maxlifetime = cf.Gclifetime
	}
	err = provider.SessionInit(cf.Maxlifetime, cf.ProviderConfig)
	if err != nil {
		return nil, err
	}
	if cf.SessionIDHashFunc == "" {
		cf.SessionIDHashFunc = "sha1"
	}
	if cf.SessionIDHashKey == "" {
		cf.SessionIDHashKey = string(generateRandomKey(16))
	}

	return &Manager{
		provider: provider,
		config:   cf,
	}, nil
}

// Start session. generate or read the session id from http request.
// if session id exists, return SessionStore with this id.
func (manager *Manager) SessionStart(w http.ResponseWriter, r *http.Request) (session SessionStore) {
	var c = appengine.NewContext(r)
	cookie, err := r.Cookie(manager.config.CookieName)
	if err != nil || cookie.Value == "" {
		sid := manager.sessionId(r)
		session, _ = manager.provider.SessionRead(sid, c)
		cookie = &http.Cookie{Name: manager.config.CookieName,
			Value:    url.QueryEscape(sid),
			Path:     "/",
			HttpOnly: true,
			Secure:   manager.config.Secure,
			Domain:   manager.config.Domain}
		if manager.config.CookieLifeTime >= 0 {
			cookie.MaxAge = manager.config.CookieLifeTime
		}
		if manager.config.EnableSetCookie {
			http.SetCookie(w, cookie)
		}
		r.AddCookie(cookie)
	} else {
		sid, _ := url.QueryUnescape(cookie.Value)
		if manager.provider.SessionExist(sid, c) {
			session, _ = manager.provider.SessionRead(sid, c)
		} else {
			sid = manager.sessionId(r)
			session, _ = manager.provider.SessionRead(sid, c)
			cookie = &http.Cookie{Name: manager.config.CookieName,
				Value:    url.QueryEscape(sid),
				Path:     "/",
				HttpOnly: true,
				Secure:   manager.config.Secure,
				Domain:   manager.config.Domain}
			if manager.config.CookieLifeTime >= 0 {
				cookie.MaxAge = manager.config.CookieLifeTime
			}
			if manager.config.EnableSetCookie {
				http.SetCookie(w, cookie)
			}
			r.AddCookie(cookie)
		}
	}
	return
}

// Destroy session by its id in http request cookie.
func (manager *Manager) SessionDestroy(w http.ResponseWriter, r *http.Request) {
	var c = appengine.NewContext(r)
	cookie, err := r.Cookie(manager.config.CookieName)
	if err != nil || cookie.Value == "" {
		return
	} else {
		manager.provider.SessionDestroy(cookie.Value, c)
		expiration := time.Now()
		cookie := http.Cookie{Name: manager.config.CookieName,
			Path:     "/",
			HttpOnly: true,
			Expires:  expiration,
			MaxAge:   -1}
		http.SetCookie(w, &cookie)
	}
}

// Get SessionStore by its id.
func (manager *Manager) GetSessionStore(sid string, c appengine.Context) (sessions SessionStore, err error) {
	sessions, err = manager.provider.SessionRead(sid, c)
	return
}

// Start session gc process.
// it can do gc in times after gc lifetime.
func (manager *Manager) GC(c appengine.Context) {
	manager.provider.SessionGC(c)
	// No such thing as a never ending goroutine...
	// Create a cronjob + custom handler instead
	// time.AfterFunc(time.Duration(manager.config.Gclifetime)*time.Second, func() { manager.GC() })
	return
}

// Regenerate a session id for this SessionStore who's id is saving in http request.
func (manager *Manager) SessionRegenerateId(w http.ResponseWriter, r *http.Request) (session SessionStore) {
	var c = appengine.NewContext(r)
	sid := manager.sessionId(r)
	cookie, err := r.Cookie(manager.config.CookieName)
	if err != nil && cookie.Value == "" {
		//delete old cookie
		session, _ = manager.provider.SessionRead(sid, c)
		cookie = &http.Cookie{Name: manager.config.CookieName,
			Value:    url.QueryEscape(sid),
			Path:     "/",
			HttpOnly: true,
			Secure:   manager.config.Secure,
			Domain:   manager.config.Domain,
		}
	} else {
		oldsid, _ := url.QueryUnescape(cookie.Value)
		session, _ = manager.provider.SessionRegenerate(oldsid, sid, c)
		cookie.Value = url.QueryEscape(sid)
		cookie.HttpOnly = true
		cookie.Path = "/"
	}
	if manager.config.CookieLifeTime >= 0 {
		cookie.MaxAge = manager.config.CookieLifeTime
	}
	http.SetCookie(w, cookie)
	r.AddCookie(cookie)
	return
}

// Get all active sessions count number.
func (manager *Manager) GetActiveSession() int {
	return manager.provider.SessionAll()
}

// Set hash function for generating session id.
func (manager *Manager) SetHashFunc(hasfunc, hashkey string) {
	manager.config.SessionIDHashFunc = hasfunc
	manager.config.SessionIDHashKey = hashkey
}

// Set cookie with https.
func (manager *Manager) SetSecure(secure bool) {
	manager.config.Secure = secure
}

// generate session id with rand string, unix nano time, remote addr by hash function.
func (manager *Manager) sessionId(r *http.Request) (sid string) {
	bs := make([]byte, 32)
	if n, err := io.ReadFull(rand.Reader, bs); n != 32 || err != nil {
		bs = utils.RandomCreateBytes(32)
	}
	sig := fmt.Sprintf("%s%d%s", r.RemoteAddr, time.Now().UnixNano(), bs)
	if manager.config.SessionIDHashFunc == "md5" {
		h := md5.New()
		h.Write([]byte(sig))
		sid = hex.EncodeToString(h.Sum(nil))
	} else if manager.config.SessionIDHashFunc == "sha1" {
		h := hmac.New(sha1.New, []byte(manager.config.SessionIDHashKey))
		fmt.Fprintf(h, "%s", sig)
		sid = hex.EncodeToString(h.Sum(nil))
	} else {
		h := hmac.New(sha1.New, []byte(manager.config.SessionIDHashKey))
		fmt.Fprintf(h, "%s", sig)
		sid = hex.EncodeToString(h.Sum(nil))
	}
	return
}
