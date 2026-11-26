package service

import (
	"fmt"
	"html"
	"strings"
	"time"
)

func buildContentModerationViolationEmailBody(siteName string, log *ContentModerationLog, cfg *ContentModerationConfig) string {
	if log == nil {
		return ""
	}
	userName := strings.TrimSpace(log.UserEmail)
	if userName == "" && log.UserID != nil {
		userName = fmt.Sprintf("UID %d", *log.UserID)
	}
	threshold := cfg.BanThreshold
	if threshold <= 0 {
		threshold = defaultContentModerationBanThreshold
	}
	statusBlock := ""
	if log.AutoBanned {
		statusBlock = emailWarning("", "账户当前处于封禁状态，所有 API 请求将被拒绝")
	}
	category := fmt.Sprintf("%s / %.3f", html.EscapeString(defaultContentModerationString(log.HighestCategory, "-")), log.HighestScore)
	site := html.EscapeString(siteName)
	return emailLayout("zh-CN", site, "账户触发内容审计规则",
		`<p>尊敬的用户 <strong>`+html.EscapeString(userName)+`</strong>，您的 API 请求在内容审计中触发平台风控策略。详情如下。</p>`+
			emailWarning("", category)+
			`<h2 style="`+emailSectionStyle+`">触发详情</h2>`+emailDetails(
			emailField{"触发时间", html.EscapeString(time.Now().Format("2006-01-02 15:04:05"))},
			emailField{"触发来源", "内容审核"},
			emailField{"所属分组", html.EscapeString(defaultContentModerationString(log.GroupName, "-"))},
			emailField{"命中类别", category},
			emailField{"累计触发次数", fmt.Sprintf("%d 次（阈值 %d）", log.ViolationCount, threshold)},
		)+statusBlock,
		"此邮件由 "+site+" 自动发送，请勿回复。")
}

func buildContentModerationAccountDisabledEmailBody(siteName string, log *ContentModerationLog, cfg *ContentModerationConfig) string {
	if log == nil {
		return ""
	}
	userName := strings.TrimSpace(log.UserEmail)
	if userName == "" && log.UserID != nil {
		userName = fmt.Sprintf("UID %d", *log.UserID)
	}
	threshold := cfg.BanThreshold
	if threshold <= 0 {
		threshold = defaultContentModerationBanThreshold
	}
	category := fmt.Sprintf("%s / %.3f", html.EscapeString(defaultContentModerationString(log.HighestCategory, "-")), log.HighestScore)
	site := html.EscapeString(siteName)
	return emailLayout("zh-CN", site, "账户已被自动禁用",
		`<p>尊敬的用户 <strong>`+html.EscapeString(userName)+`</strong>，您的账户在计数周期内多次触发平台风控策略，系统已自动禁用该账户。详情如下。</p>`+
			emailWarning("", "账户已被禁用")+
			`<h2 style="`+emailSectionStyle+`">封禁详情</h2>`+emailDetails(
			emailField{"封禁时间", html.EscapeString(time.Now().Format("2006-01-02 15:04:05"))},
			emailField{"触发来源", "内容审核"},
			emailField{"所属分组", html.EscapeString(defaultContentModerationString(log.GroupName, "-"))},
			emailField{"命中类别", category},
			emailField{"累计触发次数", fmt.Sprintf("%d 次（阈值 %d）", log.ViolationCount, threshold)},
		)+emailWarning("", "账户当前处于封禁状态，所有 API 请求将被拒绝")+`<p>如需申诉或恢复账号，请联系平台管理员处理。</p>`,
		"此邮件由 "+site+" 自动发送，请勿回复。")
}

func defaultContentModerationString(value string, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}

// buildCyberPolicyNoticeEmailBody 是 cyber_policy 通知邮件的内置兜底正文，
// 当 notification email 模板渲染失败时使用（与 sendViolationEmail 的兜底同理）。
func buildCyberPolicyNoticeEmailBody(siteName string, log *ContentModerationLog) string {
	if log == nil {
		return ""
	}
	userName := strings.TrimSpace(log.UserEmail)
	if userName == "" && log.UserID != nil {
		userName = fmt.Sprintf("UID %d", *log.UserID)
	}
	site := html.EscapeString(siteName)
	return emailLayout("zh-CN", site, "请求被网络安全策略拦截",
		`<p>尊敬的用户 <strong>`+html.EscapeString(userName)+`</strong>，您的请求被上游网络安全策略（cyber policy）拦截。</p>`+
			emailWarning("", "请求受到安全策略限制")+
			emailDetails(
				emailField{"触发时间", html.EscapeString(log.CreatedAt.Format("2006-01-02 15:04:05"))},
				emailField{"模型", html.EscapeString(defaultContentModerationString(log.Model, "-"))},
				emailField{"上游说明", emailLongText(html.EscapeString(defaultContentModerationString(log.Error, "-")))},
			)+`<p>如认为系误判，可调整请求措辞后重试，或申请获得授权的安全访问权限。</p>`,
		"此邮件由 "+site+" 自动发送，请勿回复。")
}
