package handlers

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/gin-gonic/gin"
)

func (h *SandboxHandlers) newReverseProxy(c *gin.Context, port, path string) {
	target, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%s", port))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	rp := httputil.NewSingleHostReverseProxy(target)
	if dialFn := markedDialContext(h.netMark); dialFn != nil {
		rp.Transport = &http.Transport{DialContext: dialFn}
	}
	c.Request.URL.Path = "/" + path
	c.Request.URL.RawPath = "/" + path
	c.Request.Host = target.Host
	rp.ServeHTTP(c.Writer, c.Request)
}

func (h *SandboxHandlers) ProxyGet(c *gin.Context, port, path string)    { h.newReverseProxy(c, port, path) }
func (h *SandboxHandlers) ProxyHead(c *gin.Context, port, path string)   { h.newReverseProxy(c, port, path) }
func (h *SandboxHandlers) ProxyPost(c *gin.Context, port, path string)   { h.newReverseProxy(c, port, path) }
func (h *SandboxHandlers) ProxyPut(c *gin.Context, port, path string)    { h.newReverseProxy(c, port, path) }
func (h *SandboxHandlers) ProxyPatch(c *gin.Context, port, path string)  { h.newReverseProxy(c, port, path) }
func (h *SandboxHandlers) ProxyDelete(c *gin.Context, port, path string) { h.newReverseProxy(c, port, path) }
