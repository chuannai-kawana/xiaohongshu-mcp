package xiaohongshu

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"net/url"
	"os"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

type LoginAction struct {
	page *rod.Page
}

func NewLogin(page *rod.Page) *LoginAction {
	return &LoginAction{page: page}
}

// authCheckPage returns the site page used to verify an actually signed-in
// account. It can be set to a public note URL and is restricted to XHS HTTPS.
func authCheckPage() string {
	fallback := "https://www.xiaohongshu.com/explore"
	raw := os.Getenv("XHS_AUTH_CHECK_URL")
	if raw == "" { return fallback }
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != "www.xiaohongshu.com" ||
		u.User != nil || u.Fragment != "" { return fallback }
	return raw
}

func sessionCookieValue(page *rod.Page) string {
	cs, err := page.Browser().GetCookies()
	if err != nil { return "" }
	for _, c := range cs {
		if c.Name == "web_session" { return c.Value }
	}
	return ""
}

// authenticatedUserState checks the site's actual user state rather than
// mistaking a stale web_session cookie for a valid login.
func authenticatedUserState(page *rod.Page) bool {
	r, err := page.Timeout(3 * time.Second).Eval(`() => {
		const u = window.__INITIAL_STATE__?.user?.userInfo?.value ||
			window.__INITIAL_STATE__?.user?.userInfo;
		return !!u && (u.guest === false ||
			(u.guest !== true && !!(u.userId || u.user_id || u.nickname)));
	}`)
	return err == nil && r.Value.Bool()
}

func verifyAuthenticatedPage(page *rod.Page, ctx context.Context) bool {
	pp := page.Context(ctx).Timeout(18 * time.Second)
	if err := pp.Navigate(authCheckPage()); err != nil {
		logrus.Warnf("auth check navigation failed: %v", err)
		return false
	}
	ticker := time.NewTicker(400 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(8 * time.Second)
	defer timeout.Stop()
	for {
		if authenticatedUserState(pp) { return true }
		select {
		case <-ctx.Done(): return false
		case <-timeout.C: return false
		case <-ticker.C:
		}
	}
}

func (a *LoginAction) CheckLoginStatus(ctx context.Context) (bool, error) {
	if sessionCookieValue(a.page) == "" { return false, nil }
	// The previous implementation only checked this cookie and returned
	// success for guests. Verify the user identity flag on a content page.
	return verifyAuthenticatedPage(a.page, ctx), nil
}

// CurrentUser 当前登录用户的基础信息。
type CurrentUser struct {
	Nickname string `json:"nickname"`
	UserID   string `json:"userId"`
}

// CurrentUser 从当前页面的 __INITIAL_STATE__ 读取登录用户信息。
// 需在 CheckLoginStatus 之后调用：复用已加载的 explore 页，不做额外导航。
func (a *LoginAction) CurrentUser(ctx context.Context) (*CurrentUser, error) {
	pp := a.page.Context(ctx).Timeout(10 * time.Second)

	res, err := pp.Eval(`() => {
		const u = window.__INITIAL_STATE__ && window.__INITIAL_STATE__.user;
		const info = u && u.userInfo && u.userInfo.value !== undefined ? u.userInfo.value : (u && u.userInfo);
		if (!info || info.guest) return "";
		return JSON.stringify({nickname: info.nickname, userId: info.userId || info.user_id});
	}`)
	if err != nil {
		return nil, errors.Wrap(err, "read current user state failed")
	}

	raw := res.Value.String()
	if raw == "" {
		return nil, errors.New("current user not found in page state")
	}

	var user CurrentUser
	if err := json.Unmarshal([]byte(raw), &user); err != nil {
		return nil, errors.Wrap(err, "unmarshal current user failed")
	}

	return &user, nil
}

func (a *LoginAction) Login(ctx context.Context) error {
	pp := a.page.Context(ctx)

	// 导航到小红书首页，这会触发二维码弹窗
	pp.MustNavigate("https://www.xiaohongshu.com/explore").MustWaitLoad()

	time.Sleep(2 * time.Second)

	if exists, _, _ := pp.Has(".main-container .user .link-wrapper .channel"); exists {
		return nil
	}

	pp.MustElement(".main-container .user .link-wrapper .channel")

	return nil
}

func (a *LoginAction) FetchQrcodeImage(ctx context.Context) (string, bool, error) {
	// Keep this operation below the MCP gateway timeout. Waiting for the full
	// page load can hang indefinitely on analytics/third-party resources.
	opCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	pp := a.page.Context(opCtx)
	start := time.Now()

	logrus.Info("qr login: navigating to explore")
	if err := pp.Navigate("https://www.xiaohongshu.com/login"); err != nil {
		logrus.Warnf("qr login: navigation failed after %s: %v", time.Since(start), err)
		return "", false, errors.Wrap(err, "qr login navigation failed")
	}
	logrus.Infof("qr login: navigation returned after %s; waiting for login dialog", time.Since(start))

	// Inspect only structural signals for troubleshooting; never log QR data,
	// cookies, account information or raw page HTML.
	if info, err := pp.Eval(`() => {
		const cls = (e) => typeof e.className === 'string' ? e.className.slice(0, 100) : '';
		const clickable = Array.from(document.querySelectorAll('button, a, [role="button"], span, div'))
			.filter(e => ['登录','立即登录','扫码登录','登录/注册','登录 / 注册'].includes((e.textContent||'').trim()))
			.slice(0, 12).map(e => e.tagName + '.' + cls(e));
		const imgs = Array.from(document.querySelectorAll('img'))
			.filter(e => /login|qr|code/i.test(cls(e) + ' ' + cls(e.parentElement)))
			.slice(0, 15).map(e => e.tagName + '.' + cls(e) + ' in ' + cls(e.parentElement));
		return JSON.stringify({
			path: location.pathname, ready: document.readyState,
			title: document.title.slice(0, 70),
			loginContainers: document.querySelectorAll('.login-container').length,
			qrcodeImgs: document.querySelectorAll('.qrcode-img').length,
			loginTextMatches: clickable, loginRelatedImgs: imgs,
			iframeCount: document.querySelectorAll('iframe').length
		});
	}`); err == nil {
		logrus.Infof("qr login: page structure: %s", info.Value.Str())
	} else {
		logrus.Warnf("qr login: page diagnostic failed: %v", err)
	}


	// Only inspect QR-code-looking images. Generic images could be avatars or
	// decorative icons and must not be exposed as a login QR code.
	selectors := []string{
		".login-container .qrcode-img",
		".login-container img.qrcode",
		".qrcode-container img",
	}
	didInspect := false
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		if !didInspect && time.Since(start) >= 6*time.Second {
			didInspect = true
			if v, err := pp.Eval(`() => {
				const cls = e => typeof e.className === 'string' ? e.className.slice(0,100) : '';
				const els = Array.from(document.querySelectorAll('button, [role="button"], a, input, .login-container, .login-container *'))
					.filter(e => {
						const t = (e.innerText || e.getAttribute('placeholder') || '').trim();
						return e.matches('button, input, [role="button"]') || /登录|扫码|二维码/.test(t) && t.length < 24;
					}).slice(0,40).map(e => [e.tagName,cls(e),(e.innerText||e.getAttribute('placeholder')||'').trim().slice(0,35)]);
				const imgs = Array.from(document.querySelectorAll('img')).slice(0,30).map(e => [cls(e),e.alt||'',e.naturalWidth,e.naturalHeight,cls(e.parentElement)]);
				return JSON.stringify({host:location.hostname,path:location.pathname,ready:document.readyState,items:els,images:imgs});
			}`); err == nil {
				logrus.Infof("qr login: loaded page structural diagnostic: %s", v.Value.Str())
			} else {
				logrus.Warnf("qr login: structural diagnostic failed: %v", err)
			}
		}
		if opCtx.Err() != nil {
			logrus.Warnf("qr login: no QR image after %s (dialog absent or page blocked)", time.Since(start))
			return "", false, errors.Wrap(opCtx.Err(), "qr login image not found within 45s")
		}
		if loggedIn, _, err := pp.Has(".main-container .user .link-wrapper .channel"); err == nil && loggedIn {
			logrus.Infof("qr login: already signed in after %s", time.Since(start))
			return "", true, nil
		}
		for _, selector := range selectors {
			found, el, err := pp.Has(selector)
			if err != nil || !found || el == nil {
				continue
			}
			src, err := el.Attribute("src")
			if err == nil && src != nil && len(*src) != 0 {
				logrus.Infof("qr login: QR image found after %s (selector %s)", time.Since(start), selector)
				return *src, false, nil
			}
		}
		select {
		case <-opCtx.Done():
			logrus.Warnf("qr login: timed out after %s waiting for QR", time.Since(start))
			return "", false, errors.Wrap(opCtx.Err(), "qr login image not found within 45s")
		case <-ticker.C:
		}
	}
}

// WaitForLogin monitors QR login without navigating away from the scan page.
// Mobile "login successful" can arrive before XHS hydrates its web user state.
// We retry identity verification instead of closing the session prematurely.
func (a *LoginAction) WaitForLogin(ctx context.Context) bool {
	pp := a.page.Context(ctx)
	initialCookie := sessionCookieValue(pp)
	logrus.Infof("qr login: starting monitor; initial session cookie present=%t", initialCookie != "")
	ticker := time.NewTicker(750 * time.Millisecond)
	defer ticker.Stop()
	lastState := ""
	lastVerify := time.Time{}
	verifiedAttempts := 0
	for {
		select {
		case <-ctx.Done():
			logrus.Infof("qr login: monitor ended; verification attempts=%d, last signal=%s",verifiedAttempts,lastState)
			return false
		case <-ticker.C:
			if authenticatedUserState(pp) {
				logrus.Info("qr login: authenticated web user state detected directly on QR page")
				return true
			}
			current := sessionCookieValue(pp)
			hasChanged := current != "" && current != initialCookie
			qrStage := "unknown"
			// Structural signals only: no QR values, account data, cookies or
			// page text are exposed to the log.
			if r, err := pp.Timeout(2*time.Second).Eval(`() => {
				const text = (document.body?.innerText || '');
				const img = document.querySelector('.login-container .qrcode-img');
				const qrVisible = !!img && !!(img.offsetWidth||img.offsetHeight||img.getClientRects().length);
				return JSON.stringify({
					path:location.pathname,
					qrVisible,
					scanConfirmed:/扫码成功|登录成功|已登录|已确认登录/.test(text),
					needsConfirmation:/已扫码|等待确认|请在手机上确认/.test(text),
				});
			}`); err==nil { qrStage = r.Value.Str() }
			state := fmt.Sprintf("cookiePresent=%t cookieChanged=%t qr=%s",
				current!="",hasChanged,qrStage)
			if state != lastState {
				logrus.Infof("qr login: monitor state changed: %s",state)
				lastState = state
			}
			// Don't assume an unchanged cookie means no scan; prior guest
			// sessions can already have a non-empty web_session cookie.
			// Verify after a change OR when the QR page indicates a scan.
			signal := hasChanged || strings.Contains(qrStage, "\"scanConfirmed\":true") ||
				strings.Contains(qrStage, "\"needsConfirmation\":true") ||
				strings.Contains(qrStage, "\"qrVisible\":false")
			if current == "" || !signal || time.Since(lastVerify) < 8*time.Second {
				continue
			}
			lastVerify = time.Now()
			verifiedAttempts++
			// Use a separate tab. Navigating the original QR page would
			// destroy the pending scan session after an early negative check.
			checkPage, err := pp.Browser().Page(proto.TargetCreateTarget{URL:"about:blank"})
			if err != nil {
				logrus.Warnf("qr login: verification tab creation failed: %v",err)
				continue
			}
			verified := verifyAuthenticatedPage(checkPage, ctx)
			_ = checkPage.Close()
			if verified {
				logrus.Infof("qr login: confirmed authenticated web account after %d verification attempts",verifiedAttempts)
				return true
			}
			logrus.Warn("qr login: web account still guest; keeping QR session alive for retry")
		}
	}
}
