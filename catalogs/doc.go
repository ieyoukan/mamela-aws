// Package catalogs are the catalogs of the AWS APIs the provider calls (ADR 0008 of Mamela): what
// each call does, which parameters must name the change's object (bind), which values IAM may be
// given (values), and how big an instance or a task is (measures).
//
// They are data. build.sh writes them into the Pome (pome/catalogs/*.json, by cmd/aws-catalogs),
// and Mamela uses one only when the operator names it in mamela.providers.toml
// (catalog = "pome:ec2"). The provider program never sees them: Mamela classifies every request
// the program sends with them. This Go code only writes them; the JSON is what is shipped and
// reviewed (mamela provider catalog pome).
package catalogs

import (
	"encoding/json/jsontext"
	"encoding/json/v2"

	"github.com/ieyoukan/mamela/provider/protocol"
)

// All are the catalogs, by the name the Pome ships each under.
var All = map[string]protocol.Catalog{"ec2": EC2, "ecs": ECS, "logs": Logs, "ssm": SSM, "iam": IAM}

// Encode writes a catalog file: the schema, then the catalog, indented and deterministic.
func Encode(c protocol.Catalog) ([]byte, error) {
	c.Schema = protocol.CatalogSchema
	if err := c.Check(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(c, json.Deterministic(true), jsontext.Multiline(true), jsontext.WithIndent("  "))
	return append(data, '\n'), err
}
