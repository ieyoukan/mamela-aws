// Package catalogs makes the catalogs of the AWS APIs the provider calls (ADR 0008 of Mamela): what
// each call does, which parameters must name the change's object (bind), which values IAM may be
// given (values), and how big an instance or a task is (measures).
//
// They come from AWS's official API models (Smithy, github.com/aws/api-models-aws), pinned in
// models.lock.json, and the reviewed overlay (overlay.go): the generator (cmd/aws-catalogs) checks
// every operation and parameter of the overlay, and every call the provider declares, against the
// models; takes where each parameter goes, the API's version and the X-Amz-Target from the model's
// protocol; classifies each operation by the rules of effects.go (an operation no rule covers
// fails the build); and writes pome/catalogs/*.json.
//
// They are data. Mamela uses one only when the operator names it in mamela.providers.toml
// (catalog = "pome:ec2"). The provider program never sees them: Mamela classifies every request
// the program sends with them. The JSON is what is shipped and reviewed (mamela provider catalog
// pome).
package catalogs
