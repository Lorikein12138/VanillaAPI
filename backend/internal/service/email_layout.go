package service

import "html"

const (
	emailTeal     = "#07535b"
	emailBrick    = "#a95346"
	emailFooterEN = "This is an automated message, please do not reply."
	emailFooterZH = "此邮件由系统自动发送，请勿回复。"
)

// emailLayout assembles trusted markup. Callers escape runtime text and URLs
// before passing them in, or use registry placeholders escaped by its renderer.
func emailLayout(lang, siteName, title, content, footer string) string {
	brand := ""
	if siteName != "" {
		brand = `<tr><td style="padding:16px 20px;border-bottom:1px solid #edf0e9;color:` + emailTeal + `;font-size:17px;font-weight:700;overflow-wrap:anywhere;word-break:break-word;">` + siteName + `</td></tr>`
	}
	return `<!doctype html>
<html lang="` + lang + `">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
</head>
<body style="margin:0;padding:0;background:#f5f5ef;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" bgcolor="#f5f5ef" style="width:100%;border-collapse:collapse;">
<tr><td align="center" style="padding:16px 10px;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" bgcolor="#fffefa" style="width:100%;max-width:640px;table-layout:fixed;border:1px solid #e8ebe2;border-collapse:collapse;font-family:Arial,'Microsoft YaHei',sans-serif;">
` + brand + `
<tr><td style="padding:20px;color:#405c52;font-size:15px;line-height:1.65;overflow-wrap:anywhere;word-break:break-word;">
<h1 style="margin:0 0 14px;color:#173b3b;font-size:23px;line-height:1.3;">` + title + `</h1>
` + content + `
</td></tr>
<tr><td style="padding:14px 20px;border-top:1px solid #edf0e9;color:#526d64;font-size:12px;line-height:1.5;overflow-wrap:anywhere;">` + footer + `</td></tr>
</table>
</td></tr>
</table>
</body>
</html>`
}

func emailHighlight(label, value, background, border, color, valueStyle string) string {
	labelHTML := ""
	if label != "" {
		labelHTML = `<div style="margin:0 0 4px;color:#526d64;font-size:12px;">` + label + `</div>`
	}
	return `<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="width:100%;table-layout:fixed;margin:14px 0;border-collapse:collapse;">
<tr><td bgcolor="` + background + `" style="padding:12px 14px;border:1px solid ` + border + `;overflow-wrap:anywhere;word-break:break-word;">` + labelHTML +
		`<div style="color:` + color + `;font-weight:700;` + valueStyle + `">` + value + `</div></td></tr></table>`
}

func emailPanel(label, value string) string {
	return emailHighlight(label, value, "#f1f6ef", "#e1ece0", emailTeal, "font-size:19px;")
}

func emailWarning(label, value string) string {
	return emailHighlight(label, value, "#fbf0eb", "#f0ded5", emailBrick, "font-size:19px;")
}

func emailCode(label, code string) string {
	return emailHighlight(label, code, "#f1f6ef", "#e1ece0", emailTeal, "font-family:monospace;font-size:29px;letter-spacing:.14em;")
}

const emailButtonStyle = "display:inline-block;padding:10px 18px;background:" + emailTeal + ";color:#ffffff;border-radius:4px;text-decoration:none;font-weight:700;"
const emailMutedStyle = "color:#526d64;font-size:13px;overflow-wrap:anywhere;word-break:break-word;"
const emailCellStyle = "padding:7px 6px;border-bottom:1px solid #edf0e9;vertical-align:top;overflow-wrap:anywhere;word-break:break-word;"
const emailSectionStyle = "margin:20px 0 8px;color:#173b3b;font-size:17px;line-height:1.4;"

func emailLongText(value string) string {
	return `<span style="word-break:break-all;white-space:pre-wrap;">` + value + `</span>`
}

func emailButton(url, label string) string {
	return `<p><a href="` + url + `" style="` + emailButtonStyle + `">` + label + `</a></p>`
}

type emailField struct {
	label string
	value string
}

func emailDetails(fields ...emailField) string {
	body := `<table width="100%" cellpadding="0" cellspacing="0" style="width:100%;table-layout:fixed;border-collapse:collapse;font-size:13px;">`
	for _, field := range fields {
		body += `<tr><th scope="row" align="left" width="45%" style="` + emailCellStyle + `color:#526d64;font-weight:400;">` + field.label +
			`</th><td style="` + emailCellStyle + `color:#25473f;font-weight:600;white-space:pre-wrap;">` + field.value + `</td></tr>`
	}
	return body + "</table>"
}

// BuildSMTPTestEmailBody renders the independent SMTP configuration test message.
func BuildSMTPTestEmailBody(siteName string) string {
	return emailLayout("en", html.EscapeString(siteName), "Email Configuration Successful!",
		emailPanel("", "SMTP Test Email")+
			`<p>This is a test email to verify your SMTP settings are working correctly.</p>`,
		"This is an automated test message.")
}
