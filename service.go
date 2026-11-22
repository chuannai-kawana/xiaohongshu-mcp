package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/go-rod/rod"
	"github.com/sirupsen/logrus"
	"github.com/xpzouying/headless_browser"
	"github.com/xpzouying/xiaohongshu-mcp/browser"
	"github.com/xpzouying/xiaohongshu-mcp/configs"
	"github.com/xpzouying/xiaohongshu-mcp/cookies"
	"github.com/xpzouying/xiaohongshu-mcp/pkg/downloader"
	"github.com/xpzouying/xiaohongshu-mcp/pkg/xhsutil"
	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"
)

// XiaohongshuService 小红书业务服务
type XiaohongshuService struct {
	logins loginSessions
}

// NewXiaohongshuService 创建小红书服务实例
func NewXiaohongshuService() *XiaohongshuService {
	return &XiaohongshuService{}
}

// PublishRequest 发布请求
type PublishRequest struct {
	Title      string   `json:"title" binding:"required"`
	Content    string   `json:"content" binding:"required"`
	Images     []string `json:"images" binding:"required,min=1"`
	Tags       []string `json:"tags,omitempty"`
	AtUsers    []string `json:"at_users,omitempty"`    // 需要 @ 的用户昵称列表
	ScheduleAt string   `json:"schedule_at,omitempty"` // 定时发布时间，ISO8601格式，为空则立即发布
	IsOriginal bool     `json:"is_original,omitempty"` // 是否声明原创
	Visibility string   `json:"visibility,omitempty"`  // 可见范围: "公开可见"(默认), "仅自己可见", "仅互关好友可见"
	Products   []string `json:"products,omitempty"`    // 商品关键词列表，用于绑定带货商品
}

// LoginStatusResponse 登录状态响应
type LoginStatusResponse struct {
	IsLoggedIn bool   `json:"is_logged_in"` // Cookie detected; not necessarily web-authenticated
	Username   string `json:"username,omitempty"`
	UserID     string `json:"user_id,omitempty"`
	WebAccountState string `json:"web_account_state,omitempty"`
	QRStage string `json:"qr_stage,omitempty"`
	QRActive bool `json:"qr_active,omitempty"`
	QRExpiresAt string `json:"qr_expires_at,omitempty"`
}

// LoginQrcodeResponse 登录扫码二维码
type LoginQrcodeResponse struct {
	Timeout    string `json:"timeout"`
	IsLoggedIn bool   `json:"is_logged_in"`
	Img        string `json:"img,omitempty"`
}

// PublishResponse 发布响应
type PublishResponse struct {
	Title   string   `json:"title"`
	Content string   `json:"content"`
	Images  int      `json:"images"`
	AtUsers []string `json:"at_users,omitempty"`
	Status  string   `json:"status"`
}

// PublishVideoRequest 发布视频请求（仅支持本地单个视频文件）
type PublishVideoRequest struct {
	Title      string   `json:"title" binding:"required"`
	Content    string   `json:"content" binding:"required"`
	Video      string   `json:"video" binding:"required"`
	Tags       []string `json:"tags,omitempty"`
	ScheduleAt string   `json:"schedule_at,omitempty"` // 定时发布时间，ISO8601格式，为空则立即发布
	IsOriginal bool     `json:"is_original,omitempty"` // 是否声明原创
	Visibility string   `json:"visibility,omitempty"`  // 可见范围: "公开可见"(默认), "仅自己可见", "仅互关好友可见"
	Products   []string `json:"products,omitempty"`    // 商品关键词列表，用于绑定带货商品
}

// PublishVideoResponse 发布视频响应
type PublishVideoResponse struct {
	Title   string `json:"title"`
	Content string `json:"content"`
	Video   string `json:"video"`
	Status  string `json:"status"`
}

// FeedsListResponse Feeds列表响应
type FeedsListResponse struct {
	Feeds []xiaohongshu.Feed `json:"feeds"`
	Count int                `json:"count"`
}

// UserProfileResponse 用户主页响应
type UserProfileResponse struct {
	UserBasicInfo xiaohongshu.UserBasicInfo      `json:"userBasicInfo"`
	Interactions  []xiaohongshu.UserInteractions `json:"interactions"`
	Feeds         []xiaohongshu.Feed             `json:"feeds"`
}

// DeleteCookies 删除 cookies 文件，用于登录重置
func (s *XiaohongshuService) DeleteCookies(ctx context.Context) error {
	cookiePath := cookies.GetCookiesFilePath()
	cookieLoader := cookies.NewLoadCookie(cookiePath)
	return cookieLoader.DeleteCookies()
}

// CheckLoginStatus 检查登录状态
// checkPublicWebAuth reads public page-state flags only. This is a diagnostic:
// a cookie without a web account does not prove posting permission.
func checkPublicWebAuth(ctx context.Context, page *rod.Page) string {
	raw := os.Getenv("XHS_AUTH_CHECK_URL")
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() != "www.xiaohongshu.com" {
		return "unknown"
	}
	pp := page.Context(ctx).Timeout(12*time.Second)
	if err := pp.Navigate(raw); err != nil {
		logrus.Warnf("public web account check navigation failed: %v", err)
		return "unknown"
	}
	deadline := time.Now().Add(6*time.Second)
	for time.Now().Before(deadline) {
		res, err := pp.Timeout(2*time.Second).Eval(`() => {
			const raw=window.__INITIAL_STATE__?.user?.userInfo;
			const user=raw?.value !== undefined ? raw.value : raw;
			if (!user || typeof user !== 'object') return 'unknown';
			if (user.guest === true) return 'guest';
			if (user.guest === false || !!(user.userId||user.user_id||user.nickname)) return 'authenticated';
			return 'unknown';
		}`)
		if err == nil {
			state := res.Value.Str()
			if state == "guest" || state == "authenticated" { return state }
		}
		time.Sleep(250*time.Millisecond)
	}
	return "unknown"
}

// CheckLoginStatus reports session-cookie presence and page-authentication
// separately. Pending QR sessions can be checked without opening another
// Chrome instance, which protects the one-GB service memory cap.
func (s *XiaohongshuService) CheckLoginStatus(ctx context.Context) (*LoginStatusResponse, error) {
	active, stage, expires := s.logins.snapshot()
	if active {
		return &LoginStatusResponse{
			QRStage: stage, QRActive: true, QRExpiresAt: expires.UTC().Format(time.RFC3339),
			WebAccountState: "not_checked_during_scan",
		}, nil
	}

	b := newBrowser()
	defer b.Close()
	page := b.NewPage()
	defer page.Close()
	loginAction := xiaohongshu.NewLogin(page)

	cookieDetected, err := loginAction.CheckLoginStatus(ctx)
	if err != nil { return nil, err }
	response := &LoginStatusResponse{
		IsLoggedIn: cookieDetected, QRStage: stage,
		WebAccountState: "unknown",
	}
	if cookieDetected {
		response.WebAccountState = checkPublicWebAuth(ctx, page)
		// A blank username on the login page is normal, not a reason to
		// infer an account or to expose private identifiers.
		if response.WebAccountState == "authenticated" {
			if user, err := loginAction.CurrentUser(ctx); err == nil {
				response.Username = user.Nickname
				response.UserID = user.UserID
			}
		}
	}
	return response, nil
}

// GetLoginQrcode 获取登录的扫码二维码
func (s *XiaohongshuService) GetLoginQrcode(ctx context.Context) (*LoginQrcodeResponse, error) {
	logrus.Info("qr login: initializing browser")
	b := newBrowser()
	logrus.Info("qr login: browser initialized")
	// 建页和取码都可能 panic。只有后台接手扫码后，才由它关闭浏览器。
	handedOff := false
	defer func() {
		if !handedOff {
			b.Close()
		}
	}()

	logrus.Info("qr login: creating browser page")
	page := b.NewPage()
	logrus.Info("qr login: browser page created")
	loginAction := xiaohongshu.NewLogin(page)

	img, loggedIn, err := loginAction.FetchQrcodeImage(ctx)
	if err != nil {
		return nil, err
	}

	timeout := 4 * time.Minute

	if !loggedIn {
		s.waitScanInBackground(loginAction, page, b.Close, timeout)
		handedOff = true
	}

	return &LoginQrcodeResponse{
		Timeout: func() string {
			if loggedIn {
				return "0s"
			}
			return timeout.String()
		}(),
		Img:        img,
		IsLoggedIn: loggedIn,
	}, nil
}

// waitScanInBackground 在后台等用户扫码，扫上了就存 cookie。
//
// 浏览器必须一直活着才检测得到扫码，所以这里不能提前关；但也不能任由它堆积——
// 再取一次二维码就会把上一个还在等的会话关掉，同一时刻只留一个。
func (s *XiaohongshuService) waitScanInBackground(
	loginAction *xiaohongshu.LoginAction, page *rod.Page, closeBrowser func(), timeout time.Duration,
) {
	ctxTimeout, cancel := context.WithTimeout(context.Background(), timeout)
	seq := s.logins.start(cancel, timeout)
	logrus.Infof("等待扫码登录，会话 #%d，超时 %s", seq, timeout)

	go func() {
		defer closeBrowser()
		defer cancel()
		defer s.logins.finish(seq)

		if loginAction.WaitForLoginWithStage(ctxTimeout, func(stage string) {
			s.logins.setStage(seq, stage)
		}) {
			if err := saveCookies(page); err != nil {
				s.logins.setStage(seq, "cookie_save_failed")
				logrus.Errorf("扫码后保存 cookies 失败，会话 #%d: %v", seq, err)
				return
			}
			s.logins.setStage(seq, "session_saved")
			logrus.Infof("检测到新会话并保存 cookies，会话 #%d；网页认证仍需独立验证", seq)
			return
		}

		// Expired and cancelled attempts are distinguishable from a scan
		// that reached an official SMS challenge.
		_, lastStage, _ := s.logins.snapshot()
		if lastStage == "sms_verification_required" || lastStage == "verification_required" {
			s.logins.setStage(seq, "verification_timed_out")
		} else {
			s.logins.setStage(seq, "qr_expired_or_cancelled")
		}
		logrus.Infof("登录会话 #%d 结束，最后页面状态=%s", seq, lastStage)
	}()
}

// PublishContent 发布内容
func (s *XiaohongshuService) PublishContent(ctx context.Context, req *PublishRequest) (*PublishResponse, error) {
	// 验证标题长度（小红书限制：最大20个字）
	if xhsutil.CalcTitleLength(req.Title) > 20 {
		return nil, fmt.Errorf("标题长度超过限制")
	}

	imagePaths, err := s.processImages(req.Images)
	if err != nil {
		return nil, err
	}

	var scheduleTime *time.Time
	if req.ScheduleAt != "" {
		t, err := time.Parse(time.RFC3339, req.ScheduleAt)
		if err != nil {
			return nil, fmt.Errorf("定时发布时间格式错误，请使用 ISO8601 格式: %v", err)
		}

		// 校验定时发布时间范围：1小时至14天
		now := time.Now()
		minTime := now.Add(1 * time.Hour)
		maxTime := now.Add(14 * 24 * time.Hour)

		if t.Before(minTime) {
			return nil, fmt.Errorf("定时发布时间必须至少在1小时后，当前设置: %s，最早可选: %s",
				t.Format("2006-01-02 15:04"), minTime.Format("2006-01-02 15:04"))
		}
		if t.After(maxTime) {
			return nil, fmt.Errorf("定时发布时间不能超过14天，当前设置: %s，最晚可选: %s",
				t.Format("2006-01-02 15:04"), maxTime.Format("2006-01-02 15:04"))
		}

		scheduleTime = &t
		logrus.Infof("设置定时发布时间: %s", t.Format("2006-01-02 15:04"))
	}

	content := xiaohongshu.PublishImageContent{
		Title:        req.Title,
		Content:      req.Content,
		Tags:         req.Tags,
		AtUsers:      req.AtUsers,
		ImagePaths:   imagePaths,
		ScheduleTime: scheduleTime,
		IsOriginal:   req.IsOriginal,
		Visibility:   req.Visibility,
		Products:     req.Products,
	}

	if err := s.publishContent(ctx, content); err != nil {
		logrus.Errorf("发布内容失败: title=%s %v", content.Title, err)
		return nil, err
	}

	response := &PublishResponse{
		Title:   req.Title,
		Content: req.Content,
		Images:  len(imagePaths),
		AtUsers: req.AtUsers,
		Status:  "发布完成",
	}

	return response, nil
}

// processImages 处理图片列表，支持URL下载和本地路径
func (s *XiaohongshuService) processImages(images []string) ([]string, error) {
	processor := downloader.NewImageProcessor()
	return processor.ProcessImages(images)
}

// publishContent 执行内容发布
func (s *XiaohongshuService) publishContent(ctx context.Context, content xiaohongshu.PublishImageContent) error {
	b := newBrowser()
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	action, err := xiaohongshu.NewPublishImageAction(page)
	if err != nil {
		return err
	}

	return action.Publish(ctx, content)
}

// PublishVideo 发布视频（本地文件）
func (s *XiaohongshuService) PublishVideo(ctx context.Context, req *PublishVideoRequest) (*PublishVideoResponse, error) {
	// 标题长度校验（小红书限制：最大20个字）
	if xhsutil.CalcTitleLength(req.Title) > 20 {
		return nil, fmt.Errorf("标题长度超过限制")
	}

	// 本地视频文件校验
	if req.Video == "" {
		return nil, fmt.Errorf("必须提供本地视频文件")
	}
	if _, err := os.Stat(req.Video); err != nil {
		return nil, fmt.Errorf("视频文件不存在或不可访问: %v", err)
	}

	var scheduleTime *time.Time
	if req.ScheduleAt != "" {
		t, err := time.Parse(time.RFC3339, req.ScheduleAt)
		if err != nil {
			return nil, fmt.Errorf("定时发布时间格式错误，请使用 ISO8601 格式: %v", err)
		}

		// 校验定时发布时间范围：1小时至14天
		now := time.Now()
		minTime := now.Add(1 * time.Hour)
		maxTime := now.Add(14 * 24 * time.Hour)

		if t.Before(minTime) {
			return nil, fmt.Errorf("定时发布时间必须至少在1小时后，当前设置: %s，最早可选: %s",
				t.Format("2006-01-02 15:04"), minTime.Format("2006-01-02 15:04"))
		}
		if t.After(maxTime) {
			return nil, fmt.Errorf("定时发布时间不能超过14天，当前设置: %s，最晚可选: %s",
				t.Format("2006-01-02 15:04"), maxTime.Format("2006-01-02 15:04"))
		}

		scheduleTime = &t
		logrus.Infof("设置定时发布时间: %s", t.Format("2006-01-02 15:04"))
	}

	content := xiaohongshu.PublishVideoContent{
		Title:        req.Title,
		Content:      req.Content,
		Tags:         req.Tags,
		VideoPath:    req.Video,
		ScheduleTime: scheduleTime,
		IsOriginal:   req.IsOriginal,
		Visibility:   req.Visibility,
		Products:     req.Products,
	}

	if err := s.publishVideo(ctx, content); err != nil {
		return nil, err
	}

	resp := &PublishVideoResponse{
		Title:   req.Title,
		Content: req.Content,
		Video:   req.Video,
		Status:  "发布完成",
	}
	return resp, nil
}

// publishVideo 执行视频发布
func (s *XiaohongshuService) publishVideo(ctx context.Context, content xiaohongshu.PublishVideoContent) error {
	b := newBrowser()
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	action, err := xiaohongshu.NewPublishVideoAction(page)
	if err != nil {
		return err
	}

	return action.PublishVideo(ctx, content)
}

// ListFeeds 获取Feeds列表
func (s *XiaohongshuService) ListFeeds(ctx context.Context) (*FeedsListResponse, error) {
	b := newBrowser()
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	action := xiaohongshu.NewFeedsListAction(page)

	feeds, err := action.GetFeedsList(ctx)
	if err != nil {
		logrus.Errorf("获取 Feeds 列表失败: %v", err)
		return nil, err
	}

	response := &FeedsListResponse{
		Feeds: feeds,
		Count: len(feeds),
	}

	return response, nil
}

func (s *XiaohongshuService) SearchFeeds(ctx context.Context, keyword string, filters ...xiaohongshu.FilterOption) (*FeedsListResponse, error) {
	b := newBrowser()
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	action := xiaohongshu.NewSearchAction(page)

	feeds, err := action.Search(ctx, keyword, filters...)
	if err != nil {
		return nil, err
	}

	response := &FeedsListResponse{
		Feeds: feeds,
		Count: len(feeds),
	}

	return response, nil
}

// GetFeedDetail 获取Feed详情
func (s *XiaohongshuService) GetFeedDetail(ctx context.Context, feedID, xsecToken string, loadAllComments bool) (*FeedDetailResponse, error) {
	return s.GetFeedDetailWithConfig(ctx, feedID, xsecToken, loadAllComments, xiaohongshu.DefaultCommentLoadConfig())
}

// GetFeedDetailWithConfig 使用配置获取Feed详情
func (s *XiaohongshuService) GetFeedDetailWithConfig(ctx context.Context, feedID, xsecToken string, loadAllComments bool, config xiaohongshu.CommentLoadConfig) (*FeedDetailResponse, error) {
	b := newBrowser()
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	action := xiaohongshu.NewFeedDetailAction(page)

	result, err := action.GetFeedDetailWithConfig(ctx, feedID, xsecToken, loadAllComments, config)
	if err != nil {
		return nil, err
	}

	response := &FeedDetailResponse{
		FeedID: feedID,
		Data:   result,
	}
	// Safe read-only page health signals. Never export account IDs, cookies,
	// QR values, private text, or the contents of a comment.
	if diag, e := page.Context(ctx).Timeout(4*time.Second).Eval(`() => {
		const visible = el => !!el && !!(el.offsetWidth || el.offsetHeight || el.getClientRects().length)
			&& getComputedStyle(el).visibility !== 'hidden'
			&& getComputedStyle(el).display !== 'none';
		const user = window.__INITIAL_STATE__?.user?.userInfo?.value ||
			window.__INITIAL_STATE__?.user?.userInfo || null;
		const count = selector => [...document.querySelectorAll(selector)].filter(visible).length;
		const buttons = [...document.querySelectorAll('button.submit')].slice(0,5).map(el => {
			const r=el.getBoundingClientRect();
			const hit=document.elementFromPoint(r.x+r.width/2,r.y+r.height/2);
			return {
				className:String(el.className).slice(0,60), disabled:!!el.disabled,
				visible:visible(el), parentClass:String(el.parentElement?.className||'').slice(0,60),
				hitClass:String(hit?.className||'').slice(0,60),
			};
		});
		return JSON.stringify({
			path: location.pathname,
			loginDialogCount:count('.login-container, .login-modal, [class*="login-modal"], [class*="loginModal"]'),
			loginQrCount:count('.qrcode-img, img.qrcode'),
			modalCount:count('[role="dialog"], .modal, .el-dialog'),
			guestUserState:user?.guest===true,
			hasUserState:!!user && user.guest!==true && !!(user.userId||user.user_id||user.nickname),
			commentEditorCount:count('p.content-input[contenteditable]'),
			buttons,
		});
	}`); e == nil {
		response.UIDiagnostic = diag.Value.Str()
	}
	return response, nil
}

// UserProfile 获取用户信息
func (s *XiaohongshuService) UserProfile(ctx context.Context, userID, xsecToken, tab string) (*UserProfileResponse, error) {
	parsed, err := xiaohongshu.ParseProfileTab(tab)
	if err != nil {
		return nil, err
	}

	b := newBrowser()
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	action := xiaohongshu.NewUserProfileAction(page)

	result, err := action.UserProfile(ctx, userID, xsecToken, parsed)
	if err != nil {
		return nil, err
	}
	response := &UserProfileResponse{
		UserBasicInfo: result.UserBasicInfo,
		Interactions:  result.Interactions,
		Feeds:         result.Feeds,
	}

	return response, nil

}

// PostCommentToFeed 发表评论到Feed
func (s *XiaohongshuService) PostCommentToFeed(ctx context.Context, feedID, xsecToken, content string) (*PostCommentResponse, error) {
	b := newBrowser()
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	action := xiaohongshu.NewCommentFeedAction(page)

	if err := action.PostComment(ctx, feedID, xsecToken, content); err != nil {
		return nil, err
	}

	return &PostCommentResponse{FeedID: feedID, Success: true, Message: "评论发表成功"}, nil
}

// LikeFeed 点赞笔记
func (s *XiaohongshuService) LikeFeed(ctx context.Context, feedID, xsecToken string) (*ActionResult, error) {
	b := newBrowser()
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	action := xiaohongshu.NewLikeAction(page)
	if err := action.Like(ctx, feedID, xsecToken); err != nil {
		return nil, err
	}
	return &ActionResult{FeedID: feedID, Success: true, Message: "点赞成功或已点赞"}, nil
}

// UnlikeFeed 取消点赞笔记
func (s *XiaohongshuService) UnlikeFeed(ctx context.Context, feedID, xsecToken string) (*ActionResult, error) {
	b := newBrowser()
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	action := xiaohongshu.NewLikeAction(page)
	if err := action.Unlike(ctx, feedID, xsecToken); err != nil {
		return nil, err
	}
	return &ActionResult{FeedID: feedID, Success: true, Message: "取消点赞成功或未点赞"}, nil
}

// FavoriteFeed 收藏笔记
func (s *XiaohongshuService) FavoriteFeed(ctx context.Context, feedID, xsecToken string) (*ActionResult, error) {
	b := newBrowser()
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	action := xiaohongshu.NewFavoriteAction(page)
	if err := action.Favorite(ctx, feedID, xsecToken); err != nil {
		return nil, err
	}
	return &ActionResult{FeedID: feedID, Success: true, Message: "收藏成功或已收藏"}, nil
}

// UnfavoriteFeed 取消收藏笔记
func (s *XiaohongshuService) UnfavoriteFeed(ctx context.Context, feedID, xsecToken string) (*ActionResult, error) {
	b := newBrowser()
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	action := xiaohongshu.NewFavoriteAction(page)
	if err := action.Unfavorite(ctx, feedID, xsecToken); err != nil {
		return nil, err
	}
	return &ActionResult{FeedID: feedID, Success: true, Message: "取消收藏成功或未收藏"}, nil
}

// ReplyCommentToFeed 回复指定评论
func (s *XiaohongshuService) ReplyCommentToFeed(ctx context.Context, feedID, xsecToken, commentID, userID, content string) (*ReplyCommentResponse, error) {
	b := newBrowser()
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	action := xiaohongshu.NewCommentFeedAction(page)

	if err := action.ReplyToComment(ctx, feedID, xsecToken, commentID, userID, content); err != nil {
		return nil, err
	}

	return &ReplyCommentResponse{
		FeedID:          feedID,
		TargetCommentID: commentID,
		TargetUserID:    userID,
		Success:         true,
		Message:         "评论回复成功",
	}, nil
}

// GetUnreadCount 获取通知未读数
func (s *XiaohongshuService) GetUnreadCount(ctx context.Context) (*xiaohongshu.NotificationCount, error) {
	b := newBrowser()
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	return xiaohongshu.NewNotificationAction(page).UnreadCount(ctx)
}

// ListNotifications 获取指定分区的通知列表
func (s *XiaohongshuService) ListNotifications(ctx context.Context, tab string, limit int) (*xiaohongshu.NotificationList, error) {
	parsed, err := xiaohongshu.ParseNotificationTab(tab)
	if err != nil {
		return nil, err
	}

	b := newBrowser()
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	return xiaohongshu.NewNotificationAction(page).List(ctx, parsed, limit)
}

// LikeNotification 给通知里的评论点赞或取消点赞
func (s *XiaohongshuService) LikeNotification(ctx context.Context, commentID string, unlike bool) (*xiaohongshu.NotificationLikeResult, error) {
	b := newBrowser()
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	return xiaohongshu.NewNotificationAction(page).Like(ctx, commentID, unlike)
}

// ReplyNotification 在通知页就地回复评论
func (s *XiaohongshuService) ReplyNotification(ctx context.Context, commentID, content string) (*xiaohongshu.NotificationReplyResult, error) {
	b := newBrowser()
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	return xiaohongshu.NewNotificationAction(page).Reply(ctx, commentID, content)
}

func newBrowser() *headless_browser.Browser {
	return browser.NewBrowser(configs.IsHeadless(),
		browser.WithFingerprintSeed(configs.FingerprintSeed()),
		browser.WithProxy(configs.Proxy()),
	)
}

func saveCookies(page *rod.Page) error {
	cks, err := page.Browser().GetCookies()
	if err != nil {
		return err
	}

	data, err := json.Marshal(cks)
	if err != nil {
		return err
	}

	cookieLoader := cookies.NewLoadCookie(cookies.GetCookiesFilePath())
	return cookieLoader.SaveCookies(data)
}

// withBrowserPage 执行需要浏览器页面的操作的通用函数
func withBrowserPage(fn func(*rod.Page) error) error {
	b := newBrowser()
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	return fn(page)
}

// GetMyProfile 获取当前登录用户的个人信息
func (s *XiaohongshuService) GetMyProfile(ctx context.Context, tab string) (*UserProfileResponse, error) {
	parsed, err := xiaohongshu.ParseProfileTab(tab)
	if err != nil {
		return nil, err
	}

	var result *xiaohongshu.UserProfileResponse

	err = withBrowserPage(func(page *rod.Page) error {
		action := xiaohongshu.NewUserProfileAction(page)
		result, err = action.GetMyProfileViaSidebar(ctx, parsed)
		return err
	})

	if err != nil {
		return nil, err
	}

	response := &UserProfileResponse{
		UserBasicInfo: result.UserBasicInfo,
		Interactions:  result.Interactions,
		Feeds:         result.Feeds,
	}

	return response, nil
}
