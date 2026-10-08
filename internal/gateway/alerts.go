package gateway

import (
	"fmt"
	"strings"
	"time"

	"ai-route/internal/alert"
	"ai-route/internal/store"
)

// failure feeds a failed attempt to the circuit breaker and raises alerts
// for invalid keys / arrears and for long cooldowns.
func (g *Gateway) failure(c candidate, kind failKind, retryAfter time.Duration, status int, msg string) {
	opened, cooldown := g.Breaker.Failure(c.prefix, c.target, kind, retryAfter, msg)
	switch {
	case status == 401 || status == 402:
		what := "鉴权失败（Key 无效或已被吊销）"
		if status == 402 {
			what = "欠费或额度不足"
		}
		g.Alerts.Notify(alert.Alert{Event: alert.EventAuthFailure, Subject: c.prefix,
			Title: fmt.Sprintf("供应商 %s %s", c.prefix, what),
			Text: fmt.Sprintf("上游 %s 返回 HTTP %d：%s\n整个套餐已冷却 %s，期间请求会切到其他候补。请检查 Key 或账户余额。",
				c.target, status, truncate(msg, 300), fmtDuration(cooldown))})
	case opened != "" && cooldown >= g.Alerts.LongCooldown():
		name, scope := strings.TrimPrefix(opened, "t:"), "模型"
		if strings.HasPrefix(opened, "p:") {
			name, scope = strings.TrimPrefix(opened, "p:"), "整个套餐"
		}
		g.Alerts.Notify(alert.Alert{Event: alert.EventLongCooldown, Subject: opened,
			Title: fmt.Sprintf("%s %s 冷却 %s", scope, name, fmtDuration(cooldown)),
			Text:  fmt.Sprintf("最近一次失败（%s，HTTP %d）：%s", c.target, status, truncate(msg, 300))})
	}
}

func (g *Gateway) alertAllFailed(model string, attempts []store.Attempt) {
	var lines []string
	for _, a := range attempts {
		status := "-"
		if a.HTTPStatus > 0 {
			status = fmt.Sprintf("HTTP %d", a.HTTPStatus)
		}
		lines = append(lines, fmt.Sprintf("· %s %s：%s", a.Target, status, truncate(a.Error, 150)))
	}
	g.Alerts.Notify(alert.Alert{Event: alert.EventAllFailed, Subject: model,
		Title: fmt.Sprintf("模型 %s 的全部上游都失败了", model),
		Text:  "客户端收到了错误。各次尝试：\n" + strings.Join(lines, "\n")})
}

func fmtDuration(d time.Duration) string {
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%.1f 小时", d.Hours())
	case d >= time.Minute:
		return fmt.Sprintf("%d 分钟", int(d.Minutes()+0.5))
	default:
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	}
}
