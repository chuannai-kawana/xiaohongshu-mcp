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

func (a *LoginAction) CheckLoginStatus(ctx context.Context) (bool, error) {
	// 加超时保护：只是查登录态的快速检查，不应无限挂（登录扫码的等待在 Login/WaitForLogin 里）
	pp := a.page.Context(ctx).Timeout(30 * time.Second)
	pp.MustNavigate("https://www.xiaohongshu.com/explore").MustWaitLoad()

	time.Sleep(1 * time.Second)

	exists, _, err := pp.Has(`.main-container .user .link-wrapper .channel`)
	if err != nil {
		return false, errors.Wrap(err, "check login status failed")
	}

	if !exists {
		return false, errors.Wrap(err, "login status element not found")
	}

	return true, nil
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
		".login-container img[src^='data:image/']",
		".qrcode-container img",
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
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

func (a *LoginAction) WaitForLogin(ctx context.Context) bool {
	pp := a.page.Context(ctx)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			el, err := pp.Element(".main-container .user .link-wrapper .channel")
			if err == nil && el != nil {
				return true
			}
		}
	}
}
