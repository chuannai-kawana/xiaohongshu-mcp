package xiaohongshu

import (
	"context"
	"fmt"
	"time"

	"github.com/go-rod/rod"
	"github.com/sirupsen/logrus"
	"github.com/xpzouying/xiaohongshu-mcp/humanize"
)

// CommentFeedAction 表示 Feed 评论动作
type CommentFeedAction struct {
	page *rod.Page
}

// NewCommentFeedAction 创建 Feed 评论动作
func NewCommentFeedAction(page *rod.Page) *CommentFeedAction {
	return &CommentFeedAction{page: page}
}

// PostComment 发表评论到 Feed
// findVisibleCommentElement supports both older and newer Xiaohongshu comment editors.
func findVisibleCommentElement(page *rod.Page, selectors []string, timeout time.Duration) (*rod.Element, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, selector := range selectors {
			// Do not use page.Timeout(2s).Has here: elements inherit that short
			// context, so a later humanized click fails before it completes.
			if found, el, err := page.Has(selector); err == nil && found && el != nil {
				if visible, err := el.Visible(); err == nil && visible {
					return el, nil
				}
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return nil, fmt.Errorf("not found: %v", selectors)
}

// Diagnostic deliberately avoids logging private cookies, text, or user identifiers.
func commentEditorDiagnostic(page *rod.Page) string {
	res, err := page.Timeout(3 * time.Second).Eval(`() => {
		const sels = ['.interact-container','.input-box','.content-edit',
			'p.content-input','[contenteditable="true"]','textarea',
			'.comments-container','.comment-item','button.submit'];
		const els = Array.from(document.querySelectorAll('[contenteditable], textarea')).slice(0,8)
			.map(el => ({tag:el.tagName, cls:String(el.className).slice(0,80),
				visible:!!(el.offsetWidth||el.offsetHeight||el.getClientRects().length)}));
		return JSON.stringify({path:location.pathname, counts:Object.fromEntries(
			sels.map(s=>[s,document.querySelectorAll(s).length])), editors:els});
	}`)
	if err != nil { return "unavailable: " + err.Error() }
	return res.Value.Str()
}

// PostComment writes only after verifying the editor contains exactly approved text.
func (f *CommentFeedAction) PostComment(ctx context.Context, feedID, xsecToken, content string) error {
	page := f.page.Timeout(40 * time.Second)
	url := makeFeedDetailURL(feedID, xsecToken)
	logrus.Infof("打开 feed 详情页: %s", url)
	if err := page.Navigate(url); err != nil { return fmt.Errorf("打开笔记页面失败: %w", err) }
	waitFeedPageReady(page)
	humanize.Delay(ctx, humanize.AfterNavigate)
	if err := checkPageAccessible(page); err != nil { return err }

	logrus.Infof("评论输入框诊断: %s", commentEditorDiagnostic(page))
	editors := []string{
		"div.input-box div.content-edit p.content-input",
		"div.input-box [contenteditable='true']",
		".interact-container [contenteditable='true']",
		".comment-input [contenteditable='true']",
		".comment-input-box [contenteditable='true']",
		".comments-container [contenteditable='true']",
	}
	elem, err := findVisibleCommentElement(page, editors, 5*time.Second)
	if err != nil {
		triggers := []string{
			"div.input-box div.content-edit span",
			"div.input-box div.content-edit",
			".interact-container .input-box",
			".comment-input-box",
			".comment-input",
		}
		trigger, triggerErr := findVisibleCommentElement(page, triggers, 4*time.Second)
		if triggerErr == nil {
			if err := humanize.Click(trigger); err != nil { return fmt.Errorf("打开评论输入区域失败: %w", err) }
			humanize.Delay(ctx, humanize.AfterClick)
			elem, err = findVisibleCommentElement(page, editors, 6*time.Second)
		}
		if err != nil {
			logrus.Warnf("新旧评论编辑器均未匹配: %s", commentEditorDiagnostic(page))
			return fmt.Errorf("未找到可编辑的评论输入框：请检查小红书网页端布局")
		}
	}
	// Contenteditable editors may fail WaitInteractable despite being visible.
	// Focus the editable node directly; Type also validates writability.
	if err := elem.Focus(); err != nil { return fmt.Errorf("聚焦评论输入框失败: %w", err) }
	if err := humanize.Type(ctx, elem, content); err != nil { return fmt.Errorf("输入评论失败: %w", err) }
	humanize.Delay(ctx, humanize.AfterType)
	typed, err := elem.Text()
	if err != nil || typed != content {
		return fmt.Errorf("评论输入校验失败，已取消发送（输入长度=%d，预期长度=%d）", len(typed), len(content))
	}
	logrus.Infof("评论文本已输入并校验，准备查找发布按钮: feed=%s", feedID)
	submitSelectors := []string{
		"div.bottom button.submit",
		".input-box button.submit",
		".interact-container button.submit",
		".interact-container button[class*='submit']",
		".input-box button[class*='submit']",
	}
	submit, err := findVisibleCommentElement(page, submitSelectors, 7*time.Second)
	if err != nil {
		logrus.Warnf("评论提交按钮未找到: %s", commentEditorDiagnostic(page))
		return fmt.Errorf("评论已输入，但未找到提交按钮，未发送")
	}
	// Submit is rendered and visible, but WaitInteractable can stall on
	// the comment overlay; a no-wait mouse click still checks hit geometry.
	if err := submit.ScrollIntoView(); err != nil { return fmt.Errorf("定位提交按钮失败: %w", err) }
	if err := humanize.ClickNoWait(submit); err != nil { return fmt.Errorf("点击提交按钮失败: %w", err) }
	humanize.Delay(ctx, humanize.AfterClick)
	if !waitCommentRendered(page, content, 5*time.Second) {
		logrus.Warnf("点击提交后未在评论列表确认，需人工检查是否发布: feed=%s", feedID)
		return fmt.Errorf("已点击发表评论，但暂未能确认是否成功；请先在小红书 App 检查，避免重复")
	}
	logrus.Infof("Comment posted and verified to feed: %s", feedID)
	return nil
}

// commentRendered 就地读当前页评论区 DOM，判断指定文本的评论是否已渲染出现。
// 不重新导航、不滚动——只读已加载的 .comments-container 的可见文本。
func commentRendered(page *rod.Page, content string) bool {
	res, err := page.Eval(`(txt) => {
		const items = Array.from(document.querySelectorAll('.comments-container .comment-item, .comment-item'));
		return items.some(el => el.innerText.includes(txt));
	}`, content)
	if err != nil {
		return false
	}
	return res.Value.Bool()
}

func waitCommentRendered(page *rod.Page, content string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if commentRendered(page, content) {
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

// ReplyToComment 回复指定评论
func (f *CommentFeedAction) ReplyToComment(ctx context.Context, feedID, xsecToken, commentID, userID, content string) error {
	// 增加超时时间，因为需要滚动查找评论
	// 注意：不使用 Context(ctx)，避免继承外部 context 的超时
	page := f.page.Timeout(5 * time.Minute)
	url := makeFeedDetailURL(feedID, xsecToken)
	logrus.Infof("打开 feed 详情页进行回复: %s", url)

	// 导航到详情页
	page.MustNavigate(url)
	waitFeedPageReady(page)
	humanize.Delay(ctx, humanize.AfterNavigate)

	// 检测页面是否可访问
	if err := checkPageAccessible(page); err != nil {
		return err
	}

	time.Sleep(2 * time.Second)

	// 使用 Go 实现的查找逻辑
	commentEl, err := findCommentElement(ctx, page, commentID, userID)
	if err != nil {
		return fmt.Errorf("无法找到评论: %w", err)
	}

	// 滚动到评论位置
	logrus.Info("滚动到评论位置...")
	commentEl.MustScrollIntoView()
	humanize.Delay(ctx, humanize.BetweenScroll)

	logrus.Info("准备点击回复按钮")

	// 查找并点击回复按钮
	replyBtn, err := commentEl.Element(".right .interactions .reply")
	if err != nil {
		return fmt.Errorf("无法找到回复按钮: %w", err)
	}

	if err := humanize.Click(replyBtn); err != nil {
		return fmt.Errorf("点击回复按钮失败: %w", err)
	}

	humanize.Delay(ctx, humanize.AfterClick)

	// 查找回复输入框
	inputEl, err := page.Element("div.input-box div.content-edit p.content-input")
	if err != nil {
		return fmt.Errorf("无法找到回复输入框: %w", err)
	}

	// 输入内容
	if err := humanize.Type(ctx, inputEl, content); err != nil {
		return fmt.Errorf("输入回复内容失败: %w", err)
	}

	humanize.Delay(ctx, humanize.AfterType)

	// 查找并点击提交按钮
	submitBtn, err := page.Element("div.bottom button.submit")
	if err != nil {
		return fmt.Errorf("无法找到提交按钮: %w", err)
	}

	if err := humanize.Click(submitBtn); err != nil {
		return fmt.Errorf("点击提交按钮失败: %w", err)
	}

	humanize.Delay(ctx, humanize.AfterClick)

	// 就地校验：回复应在评论区渲染出现，否则判定未成功。
	if !waitCommentRendered(page, content, 4*time.Second) {
		logrus.Warnf("回复提交后未在评论区渲染，判定未成功: feed=%s", feedID)
		return fmt.Errorf("回复未确认成功：提交后未在评论区出现（可能账号被限制或发送失败）")
	}

	logrus.Infof("回复评论成功并已确认")
	return nil
}

// findCommentElement 滚动查找指定评论：提供 commentID 时精确匹配，仅未提供时按 userID 匹配。
// 二级评论折叠在「展开 N 条回复」后面，未展开时不在 DOM 里，因此查找过程中要展开。
func findCommentElement(ctx context.Context, page *rod.Page, commentID, userID string) (*rod.Element, error) {
	logrus.Infof("开始查找评论 - commentID: %s, userID: %s", commentID, userID)

	scrollToCommentsArea(page)
	humanize.Delay(ctx, humanize.BetweenScroll)

	lastCommentCount := 0
	stagnantChecks := 0
	expandRounds := 0
	deadline := time.Now().Add(maxSearchDuration)

	// attempt 只数下滚，展开不计入，由 deadline 收口。
	for attempt := 0; attempt < maxSearchScrolls; {
		if time.Now().After(deadline) {
			logrus.Warnf("查找超过 %s，停止", maxSearchDuration)
			return nil, fmt.Errorf("评论区过大，%s 内未找到目标评论 (commentID: %s, userID: %s)",
				maxSearchDuration, commentID, userID)
		}

		// 1. 先查已渲染的评论
		if el := lookupComment(page, commentID, userID); el != nil {
			logrus.Infof("✓ 找到目标评论（下滚 %d 次）", attempt)
			return el, nil
		}

		// 2. 展开视野里的楼中楼后再查一次
		if expandRounds < maxExpandRounds {
			if expanded := expandNearbyReplies(ctx, page); expanded > 0 {
				expandRounds++
				humanize.Delay(ctx, humanize.Reading)

				if el := lookupComment(page, commentID, userID); el != nil {
					logrus.Infof("✓ 展开楼中楼后找到目标评论（下滚 %d 次）", attempt)
					return el, nil
				}

				continue
			}
		}
		expandRounds = 0
		attempt++

		// 3. 到底则不再加载
		if checkEndContainer(page) {
			logrus.Info("已到达评论底部，未找到目标评论")
			break
		}

		// 4. 评论数停滞说明加载不动了
		currentCount := getCommentCount(page)
		if currentCount != lastCommentCount {
			lastCommentCount = currentCount
			stagnantChecks = 0
		} else {
			stagnantChecks++
			if stagnantChecks >= 10 {
				logrus.Info("评论数量停滞，停止查找")
				break
			}
		}

		// 5. 滚到最后一条评论再继续下滚，触发懒加载
		if currentCount > 0 {
			if elements, err := page.Timeout(2 * time.Second).Elements(".comment-item"); err == nil && len(elements) > 0 {
				if err := elements[len(elements)-1].ScrollIntoView(); err != nil {
					logrus.Debugf("滚动到最后一条评论失败: %v", err)
				}
			}
			humanize.Delay(ctx, humanize.BetweenScroll)
		}

		humanScroll(ctx, page, "normal", false, 1)
		humanize.Delay(ctx, humanize.BetweenScroll)
	}

	return nil, fmt.Errorf("未找到评论 (commentID: %s, userID: %s)", commentID, userID)
}

// lookupComment 在当前已渲染的评论里查找目标，找不到返回 nil。
func lookupComment(page *rod.Page, commentID, userID string) *rod.Element {
	return lookupCommentTarget(commentID, userID,
		func(id string) *rod.Element {
			if el, err := page.Timeout(2 * time.Second).Element(fmt.Sprintf("#comment-%s", id)); err == nil {
				return el
			}
			return nil
		},
		func(id string) *rod.Element {
			// 一级和二级评论都带 .comment-item；.parent-comment 会把整个楼层一起匹配上。
			elements, err := page.Timeout(2 * time.Second).Elements(".comment-item")
			if err != nil {
				return nil
			}
			for _, el := range elements {
				if userEl, err := el.Timeout(500 * time.Millisecond).Element(fmt.Sprintf(`[data-user-id="%s"]`, id)); err == nil && userEl != nil {
					return el
				}
			}
			return nil
		})
}

// lookupCommentTarget 将评论定位策略与页面查询分开。
func lookupCommentTarget(commentID, userID string, findByID, findByUserID func(string) *rod.Element) *rod.Element {
	if commentID != "" {
		// 同一用户可能有多条评论；ID 未命中时留给上层展开、滚动后重查，不能按用户兜底。
		return findByID(commentID)
	}
	if userID != "" {
		return findByUserID(userID)
	}
	return nil
}
