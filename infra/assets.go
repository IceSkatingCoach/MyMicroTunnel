// SPDX-License-Identifier: GPL-3.0-or-later
// Package infra embeds the CloudFormation template into the binary, so the
// installed product carries its own copy and does not depend on a file that
// could go missing or drift out of step with the code that fills in its
// parameters.
package infra

import (
	_ "embed"
	"regexp"
	"strings"
)

//go:embed cloudformation-microtunnel.yaml
var Template string

// UpdatesTemplate is the vendor's own stack rather than a customer's: the S3
// bucket and CloudFront distribution that every shipped copy reads its update
// feed from. Embedded for the same reason as the other one — so the tool that
// fills in its parameters and the template itself cannot drift apart.
//
//go:embed cloudformation-updates.yaml
var UpdatesTemplate string

// Parameter is one of the template's inputs. Required means it has no default,
// so a deploy that does not supply it fails.
type Parameter struct {
	Name     string
	Required bool
}

var (
	topLevelKey  = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9]*):`)
	parameterKey = regexp.MustCompile(`^  ([A-Za-z][A-Za-z0-9]*):\s*$`)
	defaultKey   = regexp.MustCompile(`^    Default:`)
)

// Parameters lists what the template accepts.
//
// Scanned rather than parsed with a YAML library: the template is the one file
// in this repository that has to keep working when it is the only thing
// shipped, and a dependency pulled in to read it would be a dependency in the
// installer too. The shape it scans is enforced by the test beside it.
func Parameters() []Parameter {
	var (
		found     []Parameter
		inSection bool
		current   = -1
	)

	for _, line := range strings.Split(Template, "\n") {
		if topLevelKey.MatchString(line) {
			inSection = topLevelKey.FindStringSubmatch(line)[1] == "Parameters"
			current = -1
			continue
		}
		if !inSection {
			continue
		}
		if match := parameterKey.FindStringSubmatch(line); match != nil {
			found = append(found, Parameter{Name: match[1], Required: true})
			current = len(found) - 1
			continue
		}
		if current >= 0 && defaultKey.MatchString(line) {
			found[current].Required = false
		}
	}
	return found
}

// TemplateForDeploy is what actually goes to CloudFormation: the template
// without the prose.
//
// CloudFormation rejects a TemplateBody over 51,200 bytes, and this template
// is mostly explanation — the reasoning behind a gateway that claims its own
// Elastic IP, ten near-identical port slots, an alarm that reads silence as
// idle. That reasoning is worth keeping in the file and worth nothing to the
// service, which has been the wrong trade twice: once at 51,287 bytes and
// again on adding per-slot target ports. Shaving paragraphs to fit is a
// losing game and it costs the next reader.
//
// Only comments at the template's own indentation go. Anything indented ten
// spaces or more is inside the boot script, where a # line is content: the
// shebang above all, and the comments a person reads when they are on the
// gateway at two in the morning wondering what wrote this file.
func TemplateForDeploy() string {
	var kept []string
	for _, line := range strings.Split(Template, "\n") {
		trimmed := strings.TrimLeft(line, " ")
		if strings.HasPrefix(trimmed, "#") && len(line)-len(trimmed) < 10 {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}
