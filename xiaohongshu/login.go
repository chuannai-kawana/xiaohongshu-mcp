package xiaohongshu

import (
	"context"
	"encoding/json"
	"time"

	"github.com/go-rod/rod"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

type LoginAction struct {
	page *rod.Page
}

func NewLogin(page *rod.Page) *LoginAction {
	return &LoginAction{page: page}
}

// hasSessionCookie checks for Xiaohongshu's authenticated web session without
// exposing any cookie value to logs. A1 alone is also set for guests.
func hasSessionCookie(page *rod.Page) bool {
	cs, err := page.Browser().GetCookies()
	if err != nil {
		return false
	}
	for _, c := range cs {
		if c.Name == "web_session" && c.Value != "" {
			return true
		}
	}
	return false
}

func (a *LoginAction) CheckLoginStatus(ctx context.Context) (bool, error) {
	pp := a.page.Context(ctx).Timeout(20 * time.Second)
	// The old /explore route can redirect to international RedNote; its
	// legacy sidebar selector is not present on the Chinese login route.
	if err := pp.Navigate("https://www.xiaohongshu.com/login"); err != nil {
		return false, errors.Wrap(err, "navigate to login status page failed")
	}
	time.Sleep(2 * time.Second)
	if hasSessionCookie(pp) {
		return true, nil
	}
	// A missing session cookie is a negative result, not a navigation error.
	return false, nil
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

// LoginStage is deliberately a small, text-free signal. It does not reveal
// phone numbers, OTPs, cookies, account details or QR image data.
func currentQRLoginStage(page *rod.Page) string {
	res, err := page.Timeout(1200*time.Millisecond).Eval(`() => {
		const visible = el => {
			if (!el) return false;
			const style = getComputedStyle(el);
			return (el.offsetWidth > 0 || el.offsetHeight > 0 || el.getClientRects().length > 0)
				&& style.display !== 'none' && style.visibility !== 'hidden';
		};
		const inputs = [...document.querySelectorAll('input')].filter(visible);
		const b = document.body?.innerText || '';
		const hint = inputs.some(el => /验证码|校验码|otp|verify|one.time.code|sms|短信/i.test([
			el.placeholder,el.name,el.id,el.getAttribute('autocomplete'),el.getAttribute('aria-label')
		].filter(Boolean).join(' ')));
		const sms = /短信验证码|手机验证码|短信校验码|获取短信验证码/i.test(b);
		const generic = /安全验证|请输入验证码|验证码已发送|输入验证码|身份验证|人机验证/i.test(b) || hint;
		const phone = /已扫码|扫描成功|请在手机上确认|手机端确认|等待确认登录/i.test(b);
		const success = /登录成功|扫码登录成功|欢迎回来/i.test(b);
		const qr = [...document.querySelectorAll('.qrcode-img,.qrcode-container img')]
			.some(visible);
		const user = window.__INITIAL_STATE__?.user?.userInfo?.value ||
			window.__INITIAL_STATE__?.user?.userInfo;
		const authenticated = !!user && (user.guest === false ||
			(user.guest !== true && !!(user.userId || user.user_id || user.nickname)));
		if (authenticated) return 'web_authenticated';
		if (sms) return 'sms_verification_required';
		if (generic) return 'verification_required';
		if (phone) return 'waiting_phone_confirmation';
		if (success) return 'phone_confirmed';
		if (qr) return 'waiting_for_scan';
		return 'waiting_for_web_login';
	}`)
	if err != nil { return "waiting_for_web_login" }
	return res.Value.Str()
}

func (a *LoginAction) WaitForLogin(ctx context.Context) bool {
	return a.WaitForLoginWithStage(ctx, nil)
}

// WaitForLoginWithStage preserves the original cookie-change completion logic,
// but reports safe UI stages so SMS verification cannot be silently missed.
// It never attempts to read, submit or store verification codes.
func (a *LoginAction) WaitForLoginWithStage(ctx context.Context, stageChanged func(string)) bool {
	pp := a.page.Context(ctx)
	original := ""
	if cs, err := pp.Browser().GetCookies(); err == nil {
		for _, c := range cs {
			if c.Name == "web_session" { original = c.Value; break }
		}
	}
	logrus.Infof("qr login: waiting for new session (previous cookie present=%t)", original != "")
	ticker := time.NewTicker(750*time.Millisecond)
	defer ticker.Stop()
	lastStage := ""
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			stage := currentQRLoginStage(pp)
			if stage != lastStage {
				if stageChanged != nil { stageChanged(stage) }
				logrus.Infof("qr login: page stage=%s", stage)
				lastStage = stage
			}
			if stage == "sms_verification_required" || stage == "verification_required" {
				// A mobile prompt may be accepted but the browser is waiting
				// for an additional verification step. Do not save guest cookies.
				continue
			}
			if cs, err := pp.Browser().GetCookies(); err == nil {
				for _, c := range cs {
					if c.Name == "web_session" && c.Value != "" && c.Value != original {
						logrus.Info("qr login: new web_session detected after scan")
						if stageChanged != nil { stageChanged("session_received") }
						return true
					}
				}
			}
			if present, _, err := pp.Has(".main-container .user .link-wrapper .channel"); err == nil && present {
				logrus.Info("qr login: authenticated user sidebar detected")
				if stageChanged != nil { stageChanged("session_received") }
				return true
			}
		}
	}
}
