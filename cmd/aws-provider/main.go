// Command aws-provider is the AWS provider as a program. build.sh builds it for WASI into the Pome
// (pome/provider.wasm).
package main

import (
	"github.com/ieyoukan/mamela-aws/aws"
	"github.com/ieyoukan/mamela/provider/sdk"
)

func main() { sdk.Serve(aws.Provider) }
