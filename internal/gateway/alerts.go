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
		n := g.Alerts
		what := n.Pick("鉴权失败（Key 无效或已被吊销）", "authentication failed (key invalid or revoked)")
		if status == 402 {
			what = n.Pick("欠费或额度不足", "out of credit or quota")
		}
		dur := fmtDuration(cooldown, n.English())
		n.Notify(alert.Alert{Event: alert.EventAuthFailure, Subject: c.prefix,
			Title: fmt.Sprintf(n.Pick("供应商 %s %s", "Provider %s: %s"), c.prefix, what),
			Text: fmt.Sprintf(n.Pick("上游 %s 返回 HTTP %d：%s\n整个套餐已冷却 %s，期间请求会切到其他候补。请检查 Key 或账户余额。",
				"Upstream %s returned HTTP %d: %s\nThe whole provider is cooling down for %s; requests go to fallbacks meanwhile. Check the key or the account balance."),
				c.target, status, truncate(msg, 300), dur)})
	case opened != "" && cooldown >= g.Alerts.LongCooldown():
		n := g.Alerts
		name, scope := strings.TrimPrefix(opened, "t:"), n.Pick("模型", "Model")
		if strings.HasPrefix(opened, "p:") {
			name, scope = strings.TrimPrefix(opened, "p:"), n.Pick("整个套餐", "Provider")
		}
		n.Notify(alert.Alert{Event: alert.EventLongCooldown, Subject: opened,
			Title: fmt.Sprintf(n.Pick("%s %s 冷却 %s", "%s %s cooling down for %s"), scope, name, fmtDuration(cooldown, n.English())),
			Text:  fmt.Sprintf(n.Pick("最近一次失败（%s，HTTP %d）：%s", "Last failure (%s, HTTP %d): %s"), c.target, status, truncate(msg, 300))})
	}
}

func (g *Gateway) alertAllFailed(model string, attempts []store.Attempt) {
	var lines []string
	for _, a := range attempts {
		status := "-"
		if a.HTTPStatus > 0 {
			status = fmt.Sprintf("HTTP %d", a.HTTPStatus)
		}
		lines = append(lines, fmt.Sprintf("· %s %s: %s", a.Target, status, truncate(a.Error, 150)))
	}
	n := g.Alerts
	n.Notify(alert.Alert{Event: alert.EventAllFailed, Subject: model,
		Title: fmt.Sprintf(n.Pick("模型 %s 的全部上游都失败了", "All upstreams of model %s failed"), model),
		Text:  n.Pick("客户端收到了错误。各次尝试：\n", "The client got an error. Attempts:\n") + strings.Join(lines, "\n")})
}

func fmtDuration(d time.Duration, en bool) string {
	switch {
	case d >= time.Hour && en:
		return fmt.Sprintf("%.1f h", d.Hours())
	case d >= time.Hour:
		return fmt.Sprintf("%.1f 小时", d.Hours())
	case d >= time.Minute && en:
		return fmt.Sprintf("%d min", int(d.Minutes()+0.5))
	case d >= time.Minute:
		return fmt.Sprintf("%d 分钟", int(d.Minutes()+0.5))
	case en:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	default:
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	}
}
