// Package infra embeds the CloudFormation template into the binary, so the
// installed product carries its own copy and does not depend on a file that
// could go missing or drift out of step with the code that fills in its
// parameters.
package infra

import _ "embed"

//go:embed cloudformation-xprem-onprem-vpn.yaml
var Template string
