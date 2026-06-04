package routers

import (
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestTrustedProxyCIDRs(t *testing.T) {
	all := []string{"0.0.0.0/0", "::/0"}
	assert.Equal(t, all, trustedProxyCIDRs("*"), "star → trust all")
	assert.Equal(t, all, trustedProxyCIDRs(""), "empty → trust all")
	assert.Equal(t, all, trustedProxyCIDRs("   "), "blank → trust all")
	assert.Equal(t, []string{"10.0.0.0/8"}, trustedProxyCIDRs("10.0.0.0/8"))
	assert.Equal(t, []string{"10.0.0.0/8", "192.168.0.0/16"}, trustedProxyCIDRs("10.0.0.0/8, 192.168.0.0/16"))
}

// TestSetTrustedProxiesAcceptsResolved reproduces the boot crash: gin must
// accept whatever trustedProxyCIDRs returns (the old code passed "*" and
// crash-looped).
func TestSetTrustedProxiesAcceptsResolved(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, raw := range []string{"*", "", "10.0.0.0/8", "10.0.0.0/8,192.168.0.0/16"} {
		eng := gin.New()
		assert.NoError(t, eng.SetTrustedProxies(trustedProxyCIDRs(raw)), "raw=%q", raw)
	}
}
