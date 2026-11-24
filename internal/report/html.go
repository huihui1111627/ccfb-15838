package report

import (
	_ "embed"
	"fmt"
	"os"
	"strings"
)

//go:embed dashboard.html
var dashboardTemplate string

// WriteHTMLFile renders the self-contained dashboard next to the JSON report.
func WriteHTMLFile(path string, rep *Report) error {
	doc, err := RenderHTML(rep)
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(doc), 0o644)
}

// RenderHTML renders the dashboard as one self-contained HTML document.
// The report is embedded as JSON and drawn client-side with SVG, so the page
// works offline from the file system.
func RenderHTML(rep *Report) (string, error) {
	payload, err := jsonBytes(rep)
	if err != nil {
		return "", err
	}
	title := fmt.Sprintf("集群 DNS 仿真 — %s", rep.Config.Summary())
	out := strings.Replace(dashboardTemplate, "{{TITLE}}", htmlEscape(title), 1)
	out = strings.Replace(out, "{{DATA}}", string(payload), 1)
	return out, nil
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}
