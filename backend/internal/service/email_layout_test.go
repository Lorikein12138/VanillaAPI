//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNotificationEmailOfficialVisualTemplates(t *testing.T) {
	cases := map[string]struct {
		en, zh  string
		warning bool
		markers []string
	}{
		NotificationEmailEventAuthVerifyCode:              {"Email verification code", "邮箱验证码", false, []string{"letter-spacing:.14em;", "{{verification_code}}", "{{expires_in_minutes}}"}},
		NotificationEmailEventAuthPasswordReset:           {"Password reset", "密码重置", false, []string{`href="{{reset_url}}"`, "<br>{{reset_url}}", "{{expires_in_minutes}}"}},
		NotificationEmailEventNotificationEmailVerifyCode: {"Notification email verification", "通知邮箱验证", false, []string{"letter-spacing:.14em;", "{{verification_code}}", "{{expires_in_minutes}}"}},
		NotificationEmailEventSubscriptionPurchaseSuccess: {"Subscription activated", "订阅已开通", false, []string{"{{subscription_group}}", "{{subscription_days}}", "{{expiry_time}}", "{{order_id}}"}},
		NotificationEmailEventSubscriptionExpiryReminder:  {"Subscription expiry reminder", "订阅到期提醒", false, []string{"{{subscription_group}}", "{{days_remaining}}", "{{expiry_time}}", `href="{{unsubscribe_url}}"`}},
		NotificationEmailEventBalanceLow:                  {"Low balance alert", "余额不足提醒", false, []string{"{{site_currency_symbol}}{{current_balance}}", "{{site_currency_symbol}}{{threshold}}", `href="{{recharge_url}}"`, `href="{{unsubscribe_url}}"`}},
		NotificationEmailEventBalanceRechargeSuccess:      {"Recharge successful", "余额充值成功", false, []string{"{{site_currency_symbol}}{{recharge_amount}}", "{{site_currency_symbol}}{{current_balance}}", "{{order_id}}", `<th scope="row"`}},
		NotificationEmailEventAccountQuotaAlert:           {"Account quota alert", "账号限额告警", false, []string{"{{quota_used}} / {{quota_limit}}", "{{quota_remaining}}", "{{quota_threshold}}", `<th scope="row"`}},
		NotificationEmailEventContentModerationViolation:  {"Risk control notice", "账户风控提醒", true, []string{"{{moderation_category}} / {{moderation_score}}", "{{violation_count}} / {{ban_threshold}}"}},
		NotificationEmailEventContentModerationDisabled:   {"Account disabled", "账户已被禁用", true, []string{"{{moderation_category}} / {{moderation_score}}", "{{violation_count}} / {{ban_threshold}}"}},
		NotificationEmailEventCyberPolicyNotice:           {"Cyber-security policy notice", "网络安全策略拦截提醒", true, []string{"{{model}}", "{{upstream_message}}", "word-break:break-all"}},
		NotificationEmailEventOpsAlert:                    {"Ops alert", "运维告警", true, []string{"{{rule_name}}", "{{metric_value}}", "{{operator}} {{threshold_value}}", "{{triggered_at}}", `<th scope="row"`}},
		NotificationEmailEventOpsScheduledReport:          {"{{report_name}}", "{{report_name}}", false, []string{"{{report_total_requests}}", "{{report_sla}}", "{{report_latency_p99}}", "{{report_tps_avg}}", `display: {{report_summary_display}};`, `display: {{report_detail_display}};">{{report_html}}</div>`, `<th scope="row"`}},
	}

	ctx := context.Background()
	svc := NewNotificationEmailService(newNotificationEmailMemorySettingRepo(), nil)
	require.Len(t, cases, len(svc.ListEventInfos()))
	for _, event := range svc.ListEventInfos() {
		test, ok := cases[event.Event]
		require.True(t, ok, "missing visual coverage for %s", event.Event)
		t.Run(event.Event, func(t *testing.T) {
			en, err := svc.GetTemplate(ctx, event.Event, "en")
			require.NoError(t, err)
			zh, err := svc.GetTemplate(ctx, event.Event, "zh")
			require.NoError(t, err)
			require.ElementsMatch(t, notificationEmailPlaceholdersIn(en.Subject), notificationEmailPlaceholdersIn(zh.Subject))
			require.ElementsMatch(t, notificationEmailPlaceholdersIn(en.HTML), notificationEmailPlaceholdersIn(zh.HTML))
			for _, localized := range []struct {
				locale, lang, heading string
				template              NotificationEmailTemplate
			}{
				{"en", "en", test.en, en},
				{"zh", "zh-CN", test.zh, zh},
			} {
				t.Run(localized.locale, func(t *testing.T) {
					body := localized.template.HTML
					require.Contains(t, body, `<html lang="`+localized.lang+`">`)
					require.Contains(t, body, ">"+localized.heading+"</h1>")
					require.Contains(t, body, "{{site_name}}")
					require.Contains(t, body, `<table role="presentation" width="100%"`)
					require.Contains(t, body, "#fffefa")
					require.Contains(t, body, "#07535b")
					if test.warning {
						require.Contains(t, body, "#fbf0eb")
						require.Contains(t, body, "#a95346")
					} else {
						require.NotContains(t, body, "#fbf0eb")
					}
					for _, marker := range test.markers {
						require.Contains(t, body, marker)
					}
					require.NotContains(t, body, "<style")
					require.NotContains(t, body, "<script")
					require.NotContains(t, body, "min-width")
					preview, err := svc.PreviewTemplate(ctx, NotificationEmailPreviewInput{Event: event.Event, Locale: localized.locale})
					require.NoError(t, err)
					require.NotEmpty(t, preview.Subject)
					require.NotContains(t, preview.HTML, "{{")
				})
			}
		})
	}
}

func TestEmailLayoutLegacyAndSMTP(t *testing.T) {
	const site = `Demo <API> & Co`
	email := &EmailService{}
	balance := &BalanceNotifyService{}
	log := &ContentModerationLog{UserEmail: "demo@example.com", HighestCategory: "sample", AutoBanned: true}
	cfg := defaultContentModerationConfig()
	end := time.Date(2026, 11, 26, 4, 0, 0, 0, time.UTC)
	start := end.Add(-24 * time.Hour)
	errorDigest := buildOpsErrorDigestEmailHTML("Digest <test>", start, end, &OpsErrorLogList{
		Total:  1,
		Errors: []*OpsErrorLog{{CreatedAt: end, Platform: "demo", StatusCode: 502, Message: "<script>sample</script>"}},
	})
	for _, test := range []struct {
		name, body, marker string
		warning            bool
		hasSite            bool
	}{
		{"verify", email.buildVerifyCodeEmailBody("123456", site), "123456", false, true},
		{"reset", email.buildPasswordResetEmailBody("https://example.invalid/reset?a=1&b=2", site), `href="https://example.invalid/reset?a=1&amp;b=2"`, false, true},
		{"notify verify", buildNotifyVerifyEmailBody("654321", site), "654321", false, true},
		{"balance", balance.buildBalanceLowEmailBody("Demo", 3.14, 10, "Demo &lt;API&gt; &amp; Co", "", SiteDisplayCurrencyCNY), "¥3.14", false, true},
		{"quota", balance.buildQuotaAlertEmailBody(42, "Demo", "demo", "Daily", 80, 100, 20, "20%", "Demo &lt;API&gt; &amp; Co", "$"), "20%", false, true},
		{"violation", buildContentModerationViolationEmailBody(site, log, cfg), "账户当前处于封禁状态", true, true},
		{"disabled", buildContentModerationAccountDisabledEmailBody(site, log, cfg), "封禁详情", true, true},
		{"policy", buildCyberPolicyNoticeEmailBody(site, log), "请求被网络安全策略拦截", true, true},
		{"ops alert", buildOpsAlertEmailBody(&OpsAlertRule{Name: "Rule <test>"}, &OpsAlertEvent{FiredAt: end}), "Rule &lt;test&gt;", true, false},
		{"ops summary", buildOpsScheduledReportFallbackEmailBody(buildOpsSummaryEmailHTML("Daily", start, end, &OpsDashboardOverview{RequestCountTotal: 42})), ">42</div>", false, false},
		{"ops digest", buildOpsScheduledReportFallbackEmailBody(errorDigest), "&lt;script&gt;sample&lt;/script&gt;", true, false},
		{"ops account", buildOpsScheduledReportFallbackEmailBody(buildOpsAccountHealthEmailHTML("Accounts", start, end, nil)), "Total Accounts", false, false},
		{"SMTP", BuildSMTPTestEmailBody(site), "Email Configuration Successful!", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.Contains(t, test.body, `<table role="presentation" width="100%"`)
			require.Contains(t, test.body, "#fffefa")
			require.Contains(t, test.body, test.marker)
			require.NotContains(t, test.body, "<script")
			if test.warning {
				require.Contains(t, test.body, "#fbf0eb")
				require.Contains(t, test.body, "#a95346")
			}
			if test.hasSite {
				require.Contains(t, test.body, "Demo &lt;API&gt; &amp; Co")
			}
		})
	}
	require.Contains(t, errorDigest, `<th scope="col"`)
	require.NotContains(t, errorDigest, "<html")
}
