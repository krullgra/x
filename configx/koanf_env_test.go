package configx

import (
	_ "embed"
	"testing"

	"github.com/dgraph-io/ristretto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

//go:embed stub/kratos/config.schema.json
var kratosSchema []byte

func TestNewKoanfEnvCache(t *testing.T) {
	ref, compiler, err := newCompiler(kratosSchema)
	require.NoError(t, err)
	schema, err := compiler.Compile(ref)
	require.NoError(t, err)

	c := *schemaPathCacheConfig
	c.Metrics = true
	schemaPathCache, _ = ristretto.NewCache(&c)
	_, _ = NewKoanfEnv("", kratosSchema, schema)
	_, _ = NewKoanfEnv("", kratosSchema, schema)
	assert.EqualValues(t, 1, schemaPathCache.Metrics.Hits())
}
