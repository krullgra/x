package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"testing"

	"github.com/pkg/errors"

	"github.com/ory/x/httpx"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ory/x/urlx"
)

// This test is a full integration test for the proxy.
// It does not have to cover **all** edge cases included in the rewrite
// unit test, but should use all features like path prefix, ...

const statusTestFailure = 555

type (
	remoteT struct {
		w      http.ResponseWriter
		r      *http.Request
		t      *testing.T
		failed bool
	}
	testingRoundTripper struct {
		t  *testing.T
		rt http.RoundTripper
	}
)

func (t *remoteT) Errorf(format string, args ...interface{}) {
	t.failed = true
	t.w.WriteHeader(statusTestFailure)
	t.t.Errorf(format, args...)
}

func (t *remoteT) Header() http.Header {
	return t.w.Header()
}

func (t *remoteT) Write(i []byte) (int, error) {
	if t.failed {
		return 0, nil
	}
	return t.w.Write(i)
}

func (t *remoteT) WriteHeader(statusCode int) {
	if t.failed {
		return
	}
	t.w.WriteHeader(statusCode)
}

func (rt *testingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := rt.rt.RoundTrip(req)
	require.NoError(rt.t, err)

	if resp.StatusCode == statusTestFailure {
		rt.t.Error("got test failure from the server, see output above")
		rt.t.FailNow()
	}

	return resp, err
}

func TestFullIntegration(t *testing.T) {
	upstream, upstreamHandler := httpx.NewChanHandler(0)
	upstreamServer := httptest.NewTLSServer(upstream)
	defer upstreamServer.Close()

	// create the proxy
	hostMapper := make(chan func(*http.Request) (*HostConfig, error))
	reqMiddleware := make(chan ReqMiddleware)
	respMiddleware := make(chan RespMiddleware)

	type CustomErrorReq func(*http.Request, error)
	type CustomErrorResp func(*http.Response, error) error

	onErrorReq := make(chan CustomErrorReq)
	onErrorResp := make(chan CustomErrorResp)

	proxy := httptest.NewTLSServer(New(
		func(_ context.Context, r *http.Request) (*HostConfig, error) {
			return (<-hostMapper)(r)
		},
		WithTransport(upstreamServer.Client().Transport),
		WithReqMiddleware(func(req *http.Request, config *HostConfig, body []byte) ([]byte, error) {
			f := <-reqMiddleware
			if f == nil {
				return body, nil
			}
			return f(req, config, body)
		}),
		WithRespMiddleware(func(resp *http.Response, config *HostConfig, body []byte) ([]byte, error) {
			f := <-respMiddleware
			if f == nil {
				return body, nil
			}
			return f(resp, config, body)
		}),
		WithOnError(func(request *http.Request, err error) {
			f := <-onErrorReq
			if f == nil {
				return
			}
			f(request, err)
		}, func(response *http.Response, err error) error {
			f := <-onErrorResp
			if f == nil {
				return nil
			}
			return f(response, err)
		})))

	cl := proxy.Client()
	cl.Transport = &testingRoundTripper{t, cl.Transport}
	cl.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	for _, tc := range []struct {
		desc           string
		hostMapper     func(host string) (*HostConfig, error)
		handler        func(assert *assert.Assertions, w http.ResponseWriter, r *http.Request)
		request        func(t *testing.T) *http.Request
		assertResponse func(t *testing.T, r *http.Response)
		reqMiddleware  ReqMiddleware
		respMiddleware RespMiddleware
		onErrReq       CustomErrorReq
		onErrResp      CustomErrorResp
	}{
		{
			desc: "body replacement",
			hostMapper: func(host string) (*HostConfig, error) {
				if host != "example.com" {
					return nil, fmt.Errorf("got unexpected host %s, expected 'example.com'", host)
				}
				return &HostConfig{
					CookieDomain: "example.com",
					PathPrefix:   "/foo",
				}, nil
			},
			handler: func(assert *assert.Assertions, w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				assert.NoError(err)
				assert.Equal(fmt.Sprintf("some random content containing the request URL and path prefix %s/bar but also other stuff", upstreamServer.URL), string(body))

				_, err = w.Write([]byte(fmt.Sprintf("just responding with my own URL: %s/baz and some path of course", upstreamServer.URL)))
				assert.NoError(err)
			},
			request: func(t *testing.T) *http.Request {
				req, err := http.NewRequest(http.MethodPost, proxy.URL+"/foo", bytes.NewBufferString(fmt.Sprintf("some random content containing the request URL and path prefix %s/bar but also other stuff", upstreamServer.URL)))
				require.NoError(t, err)
				req.Host = "example.com"
				return req
			},
			assertResponse: func(t *testing.T, resp *http.Response) {
				assert.Equal(t, http.StatusOK, resp.StatusCode)

				body, err := io.ReadAll(resp.Body)
				require.NoError(t, err)
				assert.Equal(t, "just responding with my own URL: https://example.com/foo/baz and some path of course", string(body))
			},
		},
		{
			desc: "redirection replacement",
			hostMapper: func(host string) (*HostConfig, error) {
				if host != "redirect.me" {
					return nil, fmt.Errorf("got unexpected host %s, expected 'redirect.me'", host)
				}
				return &HostConfig{
					CookieDomain: "redirect.me",
				}, nil
			},
			handler: func(_ *assert.Assertions, w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, upstreamServer.URL+"/redirection/target", http.StatusSeeOther)
			},
			request: func(t *testing.T) *http.Request {
				req, err := http.NewRequest(http.MethodGet, proxy.URL, nil)
				require.NoError(t, err)
				req.Host = "redirect.me"
				return req
			},
			assertResponse: func(t *testing.T, r *http.Response) {
				assert.Equal(t, http.StatusSeeOther, r.StatusCode)
				assert.Equal(t, "https://redirect.me/redirection/target", r.Header.Get("Location"))
			},
		},
		{
			desc: "cookie replacement",
			hostMapper: func(host string) (*HostConfig, error) {
				if host != "auth.cookie.love" {
					return nil, fmt.Errorf("got unexpected host %s, expected 'cookie.love'", host)
				}
				return &HostConfig{
					CookieDomain: "cookie.love",
				}, nil
			},
			handler: func(assert *assert.Assertions, w http.ResponseWriter, r *http.Request) {
				http.SetCookie(w, &http.Cookie{
					Name:   "auth",
					Value:  "my random cookie",
					Domain: urlx.ParseOrPanic(upstreamServer.URL).Hostname(),
				})
				_, err := w.Write([]byte("OK"))
				assert.NoError(err)
			},
			request: func(t *testing.T) *http.Request {
				req, err := http.NewRequest(http.MethodGet, proxy.URL, nil)
				require.NoError(t, err)
				req.Host = "auth.cookie.love"
				return req
			},
			assertResponse: func(t *testing.T, r *http.Response) {
				cookies := r.Cookies()
				require.Len(t, cookies, 1)
				c := cookies[0]
				assert.Equal(t, "auth", c.Name)
				assert.Equal(t, "my random cookie", c.Value)
				assert.Equal(t, "cookie.love", c.Domain)
			},
		},
		{
			desc: "custom middleware",
			hostMapper: func(host string) (*HostConfig, error) {
				return &HostConfig{}, nil
			},
			handler: func(assert *assert.Assertions, w http.ResponseWriter, r *http.Request) {
				assert.Equal("noauth.example.com", r.Host)
				b, err := ioutil.ReadAll(r.Body)
				assert.NoError(err)
				assert.Equal("this is a new body", string(b))

				_, err = w.Write([]byte("OK"))
				assert.NoError(err)
			},
			request: func(t *testing.T) *http.Request {
				req, err := http.NewRequest(http.MethodPost, proxy.URL, bytes.NewReader([]byte("body")))
				require.NoError(t, err)
				req.Host = "auth.example.com"
				return req
			},
			assertResponse: func(t *testing.T, r *http.Response) {
				body, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				assert.Equal(t, "OK", string(body))
				assert.Equal(t, "1234", r.Header.Get("Some-Header"))
			},
			reqMiddleware: func(req *http.Request, config *HostConfig, body []byte) ([]byte, error) {
				req.Host = "noauth.example.com"
				body = []byte("this is a new body")
				return body, nil
			},
			respMiddleware: func(resp *http.Response, config *HostConfig, body []byte) ([]byte, error) {
				resp.Header.Add("Some-Header", "1234")
				return body, nil
			},
		},
		{
			desc: "custom request errors",
			hostMapper: func(host string) (*HostConfig, error) {
				return &HostConfig{}, errors.New("some host mapper error occurred")
			},
			handler: func(assert *assert.Assertions, w http.ResponseWriter, r *http.Request) {
				_, err := w.Write([]byte("OK"))
				assert.NoError(err)
			},
			request: func(t *testing.T) *http.Request {
				req, err := http.NewRequest(http.MethodPost, proxy.URL, bytes.NewReader([]byte("body")))
				require.NoError(t, err)
				req.Host = "auth.example.com"
				return req
			},
			assertResponse: func(t *testing.T, r *http.Response) {
				return
			},
			onErrReq: func(request *http.Request, err error) {
				assert.Error(t, err)
				assert.Equal(t, "some host mapper error occurred", err.Error())
			},
		},
		{
			desc: "custom response errors",
			hostMapper: func(host string) (*HostConfig, error) {
				return &HostConfig{}, nil
			},
			handler: func(assert *assert.Assertions, w http.ResponseWriter, r *http.Request) {
				_, err := w.Write([]byte("OK"))
				assert.NoError(err)
			},
			request: func(t *testing.T) *http.Request {
				req, err := http.NewRequest(http.MethodPost, proxy.URL, bytes.NewReader([]byte("body")))
				require.NoError(t, err)
				req.Host = "auth.example.com"
				return req
			},
			assertResponse: func(t *testing.T, r *http.Response) {
				return
			},
			respMiddleware: func(resp *http.Response, config *HostConfig, body []byte) ([]byte, error) {
				return nil, errors.New("some response middleware error")
			},
			onErrResp: func(response *http.Response, err error) error {
				assert.Error(t, err)
				assert.Equal(t, "some response middleware error", err.Error())
				return err
			},
		},
	} {
		t.Run("case="+tc.desc, func(t *testing.T) {
			go func() {
				hostMapper <- func(r *http.Request) (*HostConfig, error) {
					host := r.Host
					hc, err := tc.hostMapper(host)
					if err == nil {
						hc.UpstreamHost = urlx.ParseOrPanic(upstreamServer.URL).Host
						hc.UpstreamScheme = urlx.ParseOrPanic(upstreamServer.URL).Scheme
						hc.TargetHost = hc.UpstreamHost
						hc.TargetScheme = hc.UpstreamScheme
					}
					return hc, err
				}
				reqMiddleware <- tc.reqMiddleware
				upstreamHandler <- func(w http.ResponseWriter, r *http.Request) {
					t := &remoteT{t: t, w: w, r: r}
					tc.handler(assert.New(t), t, r)
				}
				respMiddleware <- tc.respMiddleware
			}()

			go func() {
				onErrorReq <- tc.onErrReq
			}()

			go func() {
				onErrorResp <- tc.onErrResp
			}()

			resp, err := cl.Do(tc.request(t))
			require.NoError(t, err)
			tc.assertResponse(t, resp)
		})
	}
}

func TestBetweenReverseProxies(t *testing.T) {
	// the target thinks it is running under the targetHost, while actually it is behind all three proxies
	targetHost := "foobar.ory.sh"
	targetHandler, c := httpx.NewChanHandler(1)
	target := httptest.NewServer(targetHandler)

	revProxyHandler := httputil.NewSingleHostReverseProxy(urlx.ParseOrPanic(target.URL))
	revProxy := httptest.NewServer(revProxyHandler)

	thisProxy := httptest.NewServer(New(func(ctx context.Context, _ *http.Request) (*HostConfig, error) {
		return &HostConfig{
			CookieDomain:   "sh",
			UpstreamHost:   urlx.ParseOrPanic(revProxy.URL).Host,
			UpstreamScheme: urlx.ParseOrPanic(revProxy.URL).Scheme,
			TargetScheme:   "http",
			TargetHost:     targetHost,
		}, nil
	}))

	ingressHandler := httputil.NewSingleHostReverseProxy(urlx.ParseOrPanic(thisProxy.URL))
	ingress := httptest.NewServer(ingressHandler)

	// In this scenario we want to force the use of the X-Forwarded-Host header instead of the Host header.
	singleHostDirector := ingressHandler.Director
	ingressHandler.Director = func(req *http.Request) {
		singleHostDirector(req)
		req.Header.Set("X-Forwarded-Host", req.Host)
		req.Host = urlx.ParseOrPanic(ingress.URL).Host
	}

	t.Run("case=replaces body", func(t *testing.T) {
		const pattern = "Hello, I am available under http://%s!"
		c <- func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, pattern, targetHost)
		}

		host := "example.com"
		req, err := http.NewRequest(http.MethodGet, ingress.URL, nil)
		require.NoError(t, err)
		req.Host = host

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, fmt.Sprintf(pattern, host), string(body))
	})

	t.Run("case=replaces cookies", func(t *testing.T) {
		c <- func(w http.ResponseWriter, r *http.Request) {
			http.SetCookie(w, &http.Cookie{
				Name:   "foo",
				Value:  "setting this cookie for my own domain",
				Domain: targetHost,
				Secure: true,
			})
		}

		req, err := http.NewRequest(http.MethodGet, ingress.URL, nil)
		require.NoError(t, err)
		req.Host = "example.com"

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)

		cookies := resp.Cookies()
		require.Len(t, cookies, 1)
		assert.Equal(t, "foo", cookies[0].Name)
		assert.Equal(t, "setting this cookie for my own domain", cookies[0].Value)
		assert.Equal(t, "sh", cookies[0].Domain)
		assert.Equal(t, false, cookies[0].Secure)
	})

	t.Run("case=replaces location", func(t *testing.T) {
		c <- func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://"+targetHost, http.StatusSeeOther)
		}

		host := "example.com"
		req, err := http.NewRequest(http.MethodGet, ingress.URL, nil)
		require.NoError(t, err)
		req.Host = host

		resp, err := (&http.Client{
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}).Do(req)
		require.NoError(t, err)

		assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
		assert.Equal(t, "http://"+host, resp.Header.Get("Location"))
	})
}

func TestProxyProtoMix(t *testing.T) {
	const exposedHost = "foo.bar"

	setup := func(t *testing.T, targetServerFunc, upstreamServerFunc func(http.Handler) *httptest.Server) (chan<- http.HandlerFunc, string, string, *http.Client) {
		targetHandler, targetHandlerC := httpx.NewChanHandler(1)
		targetServer := targetServerFunc(targetHandler)

		upstream := httputil.NewSingleHostReverseProxy(urlx.ParseOrPanic(targetServer.URL))
		upstream.Transport = targetServer.Client().Transport
		upstreamServer := upstreamServerFunc(upstream)

		proxy := httptest.NewServer(New(func(ctx context.Context, r *http.Request) (*HostConfig, error) {
			return &HostConfig{
				CookieDomain:   exposedHost,
				UpstreamHost:   urlx.ParseOrPanic(upstreamServer.URL).Host,
				UpstreamScheme: urlx.ParseOrPanic(upstreamServer.URL).Scheme,
				TargetHost:     urlx.ParseOrPanic(targetServer.URL).Host,
				TargetScheme:   urlx.ParseOrPanic(targetServer.URL).Scheme,
			}, nil
		}, WithTransport(upstreamServer.Client().Transport)))
		client := proxy.Client()
		client.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}

		return targetHandlerC, targetServer.URL, proxy.URL, client
	}

	for _, tc := range []struct {
		name                               string
		newUpstreamServer, newTargetServer func(http.Handler) *httptest.Server
	}{
		{
			name:              "upstream http, target https",
			newUpstreamServer: httptest.NewServer,
			newTargetServer:   httptest.NewTLSServer,
		},
		{
			name:              "upstream https, target http",
			newUpstreamServer: httptest.NewTLSServer,
			newTargetServer:   httptest.NewServer,
		},
	} {
		t.Run("case="+tc.name, func(t *testing.T) {
			handler, targetURL, proxyURL, client := setup(t, httptest.NewTLSServer, httptest.NewServer)

			t.Run("case=redirect", func(t *testing.T) {
				handler <- func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, targetURL+"/see-other", http.StatusSeeOther)
				}

				req, err := http.NewRequest(http.MethodGet, proxyURL, nil)
				require.NoError(t, err)
				req.Host = exposedHost

				resp, err := client.Do(req)
				require.NoError(t, err)
				assert.Equal(t, "http://"+exposedHost+"/see-other", resp.Header.Get("Location"))
			})

			t.Run("case=body rewrite", func(t *testing.T) {
				const template = "Hello, I am %s, who are you?"

				handler <- func(w http.ResponseWriter, r *http.Request) {
					_, _ = w.Write([]byte(fmt.Sprintf(template, targetURL)))
				}

				req, err := http.NewRequest(http.MethodGet, proxyURL, nil)
				require.NoError(t, err)
				req.Host = exposedHost

				resp, err := client.Do(req)
				require.NoError(t, err)
				body, err := io.ReadAll(resp.Body)
				require.NoError(t, err)
				assert.Equal(t, fmt.Sprintf(template, "http://"+exposedHost), string(body))
			})

			t.Run("case=secure cookies", func(t *testing.T) {
				handler <- func(w http.ResponseWriter, r *http.Request) {
					cookie := &http.Cookie{
						Name:   "foo",
						Value:  "bar",
						Domain: stripPort(urlx.ParseOrPanic(targetURL).Host),
						Secure: true,
					}
					http.SetCookie(w, cookie)
					_, _ = w.Write([]byte("please eat this cookie"))
				}

				req, err := http.NewRequest(http.MethodGet, proxyURL, nil)
				require.NoError(t, err)
				req.Host = exposedHost

				resp, err := client.Do(req)
				require.NoError(t, err)

				cookies := resp.Cookies()
				require.Len(t, cookies, 1)
				assert.Equal(t, "foo", cookies[0].Name)
				assert.Equal(t, "bar", cookies[0].Value)
				assert.Equal(t, exposedHost, cookies[0].Domain)
				assert.Equal(t, false, cookies[0].Secure)
			})
		})
	}
}
