package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"opencode-telegram/internal/occlient"
)

func (a *App) showUsage(chatID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	directory := ""
	if sessionID := a.binding(chatID); sessionID != "" {
		if sess, err := a.oc.GetSession(ctx, sessionID); err == nil {
			directory = sess.Location.Directory
			a.rememberSession(sess)
		}
	}

	limits, err := a.oc.UsageLimits(ctx, directory)
	if err != nil {
		a.send(chatID, fmt.Sprintf("❌ Could not read usage limits: %v", err))
		return
	}
	if limits == nil {
		a.send(chatID,
			"⚠️ **Usage limits are unavailable.**\n\n"+
				"Sign in to an OpenCode Console workspace or check your OpenCode configuration.")
		return
	}

	var b strings.Builder
	b.WriteString("**📊 OpenCode usage limits**\n\n")
	workspace := limits.Workspace.Name
	if strings.TrimSpace(workspace) == "" {
		workspace = "OpenCode Console"
	}
	fmt.Fprintf(&b, "Workspace: **%s**\n", workspace)
	if limits.Workspace.UsageURL != "" {
		fmt.Fprintf(&b, "[Open usage dashboard](%s)\n", limits.Workspace.UsageURL)
	}

	if limits.Go != nil {
		b.WriteString("\n**OpenCode Go**\n\n")
		b.WriteString("| Window | Used | Resets |\n")
		b.WriteString("|:-------|-----:|:-------|\n")
		writeUsageMeter(&b, "Rolling", limits.Go.Rolling)
		writeUsageMeter(&b, "Weekly", limits.Go.Weekly)
		writeUsageMeter(&b, "Monthly", limits.Go.Monthly)
	}

	b.WriteString("\n**Console budgets**\n\n")
	if len(limits.Budgets) == 0 {
		b.WriteString("No budgets configured.\n")
	} else {
		b.WriteString("| Budget | Spent | Limit | Resets |\n")
		b.WriteString("|:-------|------:|------:|:-------|\n")
		for _, budget := range limits.Budgets {
			name := budget.ID
			if name == "" {
				name = "budget"
			}
			fmt.Fprintf(&b, "| %s | $%.2f | $%.2f | %s |\n",
				escapeTable(name), budget.Spent, budget.Limit, usageReset(budget.ResetsAt))
		}
	}

	a.send(chatID, b.String())
}

func writeUsageMeter(b *strings.Builder, label string, meter occlient.UsageMeter) {
	used := fmt.Sprintf("%.0f%%", meter.Percent)
	if meter.Limited {
		used = "⚠️ " + used + " (limited)"
	}
	fmt.Fprintf(b, "| %s | %s | %s |\n", label, used, usageReset(meter.ResetsAt))
}

func usageReset(ms int64) string {
	if ms <= 0 {
		return "unknown"
	}
	return time.UnixMilli(ms).Local().Format("2006-01-02 15:04 MST")
}

func escapeTable(value string) string {
	return strings.ReplaceAll(value, "|", "\\|")
}
