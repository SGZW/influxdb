package pkger_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"strings"
	"testing"

	"github.com/go-chi/chi"
	fluxurl "github.com/influxdata/flux/dependencies/url"
	"github.com/influxdata/influxdb/v2"
	pcontext "github.com/influxdata/influxdb/v2/context"
	"github.com/influxdata/influxdb/v2/kit/platform"
	influxerror "github.com/influxdata/influxdb/v2/kit/platform/errors"
	kithttp "github.com/influxdata/influxdb/v2/kit/transport/http"
	"github.com/influxdata/influxdb/v2/mock"
	"github.com/influxdata/influxdb/v2/pkg/testttp"
	"github.com/influxdata/influxdb/v2/pkger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"gopkg.in/yaml.v3"
)

func TestPkgerHTTPServerTemplate(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		b, err := ioutil.ReadFile(strings.TrimPrefix(r.URL.Path, "/"))
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Write(b)
	})
	filesvr := httptest.NewServer(mux)
	defer filesvr.Close()

	defaultClient := pkger.NewDefaultHTTPClient(fluxurl.PassValidator{})

	newPkgURL := func(t *testing.T, svrURL string, pkgPath string) string {
		t.Helper()

		u, err := url.Parse(svrURL)
		require.NoError(t, err)
		u.Path = path.Join(u.Path, pkgPath)
		return u.String()
	}

	strPtr := func(s string) *string {
		return &s
	}

	t.Run("create pkg", func(t *testing.T) {
		t.Run("should successfully return with valid req body", func(t *testing.T) {
			fakeLabelSVC := mock.NewLabelService()
			fakeLabelSVC.FindLabelByIDFn = func(ctx context.Context, id platform.ID) (*influxdb.Label, error) {
				return &influxdb.Label{
					ID: id,
				}, nil
			}
			svc := pkger.NewService(pkger.WithLabelSVC(fakeLabelSVC))
			pkgHandler := pkger.NewHTTPServerTemplates(zap.NewNop(), svc, defaultClient)
			svr := newMountedHandler(pkgHandler, 1)

			testttp.
				PostJSON(t, "/api/v2/templates/export", pkger.ReqExport{
					Resources: []pkger.ResourceToClone{
						{
							Kind: pkger.KindLabel,
							ID:   1,
							Name: "new name",
						},
					},
				}).
				Headers("Content-Type", "application/json").
				Do(svr).
				ExpectStatus(http.StatusOK).
				ExpectBody(func(buf *bytes.Buffer) {
					pkg, err := pkger.Parse(pkger.EncodingJSON, pkger.FromReader(buf))
					require.NoError(t, err)

					require.NotNil(t, pkg)
					require.NoError(t, pkg.Validate())

					assert.Len(t, pkg.Objects, 1)
					assert.Len(t, pkg.Summary().Labels, 1)
				})

		})

		t.Run("should be invalid if not org ids or resources provided", func(t *testing.T) {
			pkgHandler := pkger.NewHTTPServerTemplates(zap.NewNop(), nil, defaultClient)
			svr := newMountedHandler(pkgHandler, 1)

			testttp.
				PostJSON(t, "/api/v2/templates/export", pkger.ReqExport{}).
				Headers("Content-Type", "application/json").
				Do(svr).
				ExpectStatus(http.StatusUnprocessableEntity)

		})
	})

	t.Run("dry run pkg", func(t *testing.T) {
		t.Run("jsonnet disabled", func(t *testing.T) {
			tests := []struct {
				name        string
				contentType string
				reqBody     pkger.ReqApply
			}{
				{
					name:        "app jsonnet disabled",
					contentType: "application/x-jsonnet",
					reqBody: pkger.ReqApply{
						DryRun:      true,
						OrgID:       platform.ID(9000).String(),
						RawTemplate: bucketPkgKinds(t, pkger.EncodingJsonnet),
					},
				},
				{
					name: "retrieves package from a URL (jsonnet disabled)",
					reqBody: pkger.ReqApply{
						DryRun: true,
						OrgID:  platform.ID(9000).String(),
						Remotes: []pkger.ReqTemplateRemote{{
							URL: newPkgURL(t, filesvr.URL, "testdata/bucket_associates_labels_one.jsonnet"),
						}},
					},
				},
				{
					name:        "app json with jsonnet disabled remote",
					contentType: "application/json",
					reqBody: pkger.ReqApply{
						DryRun:      true,
						OrgID:       platform.ID(9000).String(),
						RawTemplate: bucketPkgJsonWithJsonnetRemote(t),
					},
				},
			}

			for _, tt := range tests {
				fn := func(t *testing.T) {
					svc := &fakeSVC{
						dryRunFn: func(ctx context.Context, orgID, userID platform.ID, opts ...pkger.ApplyOptFn) (pkger.ImpactSummary, error) {
							var opt pkger.ApplyOpt
							for _, o := range opts {
								o(&opt)
							}
							pkg, err := pkger.Combine(opt.Templates)
							if err != nil {
								return pkger.ImpactSummary{}, err
							}

							if err := pkg.Validate(); err != nil {
								return pkger.ImpactSummary{}, err
							}
							sum := pkg.Summary()
							var diff pkger.Diff
							for _, b := range sum.Buckets {
								diff.Buckets = append(diff.Buckets, pkger.DiffBucket{
									DiffIdentifier: pkger.DiffIdentifier{
										MetaName: b.Name,
									},
								})
							}
							return pkger.ImpactSummary{
								Summary: sum,
								Diff:    diff,
							}, nil
						},
					}

					core, sink := observer.New(zap.InfoLevel)
					pkgHandler := pkger.NewHTTPServerTemplates(zap.New(core), svc, defaultClient)
					svr := newMountedHandler(pkgHandler, 1)

					ctx := context.Background()
					testttp.
						PostJSON(t, "/api/v2/templates/apply", tt.reqBody).
						Headers("Content-Type", tt.contentType).
						WithCtx(ctx).
						Do(svr).
						ExpectStatus(http.StatusUnprocessableEntity).
						ExpectBody(func(buf *bytes.Buffer) {
							var resp pkger.RespApply
							decodeBody(t, buf, &resp)

							assert.Len(t, resp.Summary.Buckets, 0)
							assert.Len(t, resp.Diff.Buckets, 0)
						})

					// Verify logging when jsonnet is disabled
					entries := sink.TakeAll() // resets to 0
					if tt.contentType == "application/x-jsonnet" {
						require.Equal(t, 1, len(entries))
						// message 0
						require.Equal(t, zap.ErrorLevel, entries[0].Entry.Level)
						require.Equal(t, "api error encountered", entries[0].Entry.Message)
						assert.ElementsMatch(t, []zap.Field{
							zap.Error(&influxerror.Error{
								Code: influxerror.EUnprocessableEntity,
								Msg:  "template from source(s) had an issue: invalid encoding provided",
							},
							)}, entries[0].Context)
					} else if len(tt.reqBody.Remotes) == 1 && strings.HasSuffix(tt.reqBody.Remotes[0].URL, "jsonnet") {
						require.Equal(t, 1, len(entries))
						// message 0
						require.Equal(t, zap.ErrorLevel, entries[0].Entry.Level)
						require.Equal(t, "api error encountered", entries[0].Entry.Message)
						expMsg := fmt.Sprintf("template from url[\"%s\"] had an issue: invalid encoding provided", tt.reqBody.Remotes[0].URL)
						assert.ElementsMatch(t, []zap.Field{
							zap.Error(&influxerror.Error{
								Code: influxerror.EUnprocessableEntity,
								Msg:  expMsg,
							},
							)}, entries[0].Context)
					} else if len(tt.reqBody.RawTemplate.Sources) == 1 && strings.HasSuffix(tt.reqBody.RawTemplate.Sources[0], "jsonnet") {
						require.Equal(t, 1, len(entries))
						// message 0
						require.Equal(t, zap.ErrorLevel, entries[0].Entry.Level)
						require.Equal(t, "api error encountered", entries[0].Entry.Message)
						expMsg := fmt.Sprintf("template from url[\"%s\"] had an issue: invalid encoding provided", tt.reqBody.RawTemplate.Sources[0])
						assert.ElementsMatch(t, []zap.Field{
							zap.Error(&influxerror.Error{
								Code: influxerror.EUnprocessableEntity,
								Msg:  expMsg,
							},
							)}, entries[0].Context)
					} else {
						require.Equal(t, 0, len(entries))
					}
				}
				t.Run(tt.name, fn)
			}
		})

		t.Run("json", func(t *testing.T) {
			tests := []struct {
				name        string
				contentType string
				reqBody     pkger.ReqApply
			}{
				{
					name:        "app json",
					contentType: "application/json",
					reqBody: pkger.ReqApply{
						DryRun:      true,
						OrgID:       platform.ID(9000).String(),
						RawTemplate: bucketPkgKinds(t, pkger.EncodingJSON),
					},
				},
				{
					name: "defaults json when no content type",
					reqBody: pkger.ReqApply{
						DryRun:      true,
						OrgID:       platform.ID(9000).String(),
						RawTemplate: bucketPkgKinds(t, pkger.EncodingJSON),
					},
				},
				{
					name: "retrieves package from a URL (json)",
					reqBody: pkger.ReqApply{
						DryRun: true,
						OrgID:  platform.ID(9000).String(),
						Remotes: []pkger.ReqTemplateRemote{{
							URL: newPkgURL(t, filesvr.URL, "testdata/remote_bucket.json"),
						}},
					},
				},
			}

			for _, tt := range tests {
				fn := func(t *testing.T) {
					svc := &fakeSVC{
						dryRunFn: func(ctx context.Context, orgID, userID platform.ID, opts ...pkger.ApplyOptFn) (pkger.ImpactSummary, error) {
							var opt pkger.ApplyOpt
							for _, o := range opts {
								o(&opt)
							}
							pkg, err := pkger.Combine(opt.Templates)
							if err != nil {
								return pkger.ImpactSummary{}, err
							}

							if err := pkg.Validate(); err != nil {
								return pkger.ImpactSummary{}, err
							}
							sum := pkg.Summary()
							var diff pkger.Diff
							for _, b := range sum.Buckets {
								diff.Buckets = append(diff.Buckets, pkger.DiffBucket{
									DiffIdentifier: pkger.DiffIdentifier{
										MetaName: b.Name,
									},
								})
							}
							return pkger.ImpactSummary{
								Summary: sum,
								Diff:    diff,
							}, nil
						},
					}

					core, _ := observer.New(zap.InfoLevel)
					pkgHandler := pkger.NewHTTPServerTemplates(zap.New(core), svc, defaultClient)
					svr := newMountedHandler(pkgHandler, 1)

					testttp.
						PostJSON(t, "/api/v2/templates/apply", tt.reqBody).
						Headers("Content-Type", tt.contentType).
						Do(svr).
						ExpectStatus(http.StatusOK).
						ExpectBody(func(buf *bytes.Buffer) {
							var resp pkger.RespApply
							decodeBody(t, buf, &resp)

							assert.Len(t, resp.Summary.Buckets, 1)
							assert.Len(t, resp.Diff.Buckets, 1)
						})
				}
				t.Run(tt.name, fn)
			}
		})

		t.Run("yml", func(t *testing.T) {
			tests := []struct {
				name        string
				contentType string
			}{
				{
					name:        "app yml",
					contentType: "application/x-yaml",
				},
				{
					name:        "text yml",
					contentType: "text/yml",
				},
			}

			for _, tt := range tests {
				fn := func(t *testing.T) {
					svc := &fakeSVC{
						dryRunFn: func(ctx context.Context, orgID, userID platform.ID, opts ...pkger.ApplyOptFn) (pkger.ImpactSummary, error) {
							var opt pkger.ApplyOpt
							for _, o := range opts {
								o(&opt)
							}
							pkg, err := pkger.Combine(opt.Templates)
							if err != nil {
								return pkger.ImpactSummary{}, err
							}

							if err := pkg.Validate(); err != nil {
								return pkger.ImpactSummary{}, err
							}
							sum := pkg.Summary()
							var diff pkger.Diff
							for _, b := range sum.Buckets {
								diff.Buckets = append(diff.Buckets, pkger.DiffBucket{
									DiffIdentifier: pkger.DiffIdentifier{
										MetaName: b.Name,
									},
								})
							}
							return pkger.ImpactSummary{
								Diff:    diff,
								Summary: sum,
							}, nil
						},
					}

					pkgHandler := pkger.NewHTTPServerTemplates(zap.NewNop(), svc, defaultClient)
					svr := newMountedHandler(pkgHandler, 1)

					body := newReqApplyYMLBody(t, platform.ID(9000), true)

					testttp.
						Post(t, "/api/v2/templates/apply", body).
						Headers("Content-Type", tt.contentType).
						Do(svr).
						ExpectStatus(http.StatusOK).
						ExpectBody(func(buf *bytes.Buffer) {
							var resp pkger.RespApply
							decodeBody(t, buf, &resp)

							assert.Len(t, resp.Summary.Buckets, 1)
							assert.Len(t, resp.Diff.Buckets, 1)
						})
				}

				t.Run(tt.name, fn)
			}
		})

		t.Run("all diff and summary resource collections are non null", func(t *testing.T) {
			svc := &fakeSVC{
				dryRunFn: func(ctx context.Context, orgID, userID platform.ID, opts ...pkger.ApplyOptFn) (pkger.ImpactSummary, error) {
					// return zero value pkg
					return pkger.ImpactSummary{}, nil
				},
			}

			pkgHandler := pkger.NewHTTPServerTemplates(zap.NewNop(), svc, defaultClient)
			svr := newMountedHandler(pkgHandler, 1)

			testttp.
				PostJSON(t, "/api/v2/templates/apply", pkger.ReqApply{
					DryRun:      true,
					OrgID:       platform.ID(1).String(),
					RawTemplate: bucketPkgKinds(t, pkger.EncodingJSON),
				}).
				Headers("Content-Type", "application/json").
				Do(svr).
				ExpectStatus(http.StatusOK).
				ExpectBody(func(buf *bytes.Buffer) {
					var resp pkger.RespApply
					decodeBody(t, buf, &resp)
					assertNonZeroApplyResp(t, resp)
				})
		})

		t.Run("with multiple pkgs", func(t *testing.T) {
			newBktPkg := func(t *testing.T, bktName string) pkger.ReqRawTemplate {
				t.Helper()

				pkgStr := fmt.Sprintf(`[
  {
    "apiVersion": "%[1]s",
    "kind": "Bucket",
    "metadata": {
      "name": %q
    },
    "spec": {}
  }
]`, pkger.APIVersion, bktName)

				pkg, err := pkger.Parse(pkger.EncodingJSON, pkger.FromString(pkgStr))
				require.NoError(t, err)

				pkgBytes, err := pkg.Encode(pkger.EncodingJSON)
				require.NoError(t, err)
				return pkger.ReqRawTemplate{
					ContentType: pkger.EncodingJSON.String(),
					Sources:     pkg.Sources(),
					Template:    pkgBytes,
				}
			}

			tests := []struct {
				name         string
				reqBody      pkger.ReqApply
				expectedBkts []string
			}{
				{
					name: "retrieves package from a URL and raw pkgs",
					reqBody: pkger.ReqApply{
						DryRun: true,
						OrgID:  platform.ID(9000).String(),
						Remotes: []pkger.ReqTemplateRemote{{
							ContentType: "json",
							URL:         newPkgURL(t, filesvr.URL, "testdata/remote_bucket.json"),
						}},
						RawTemplates: []pkger.ReqRawTemplate{
							newBktPkg(t, "bkt1"),
							newBktPkg(t, "bkt2"),
							newBktPkg(t, "bkt3"),
						},
					},
					expectedBkts: []string{"bkt1", "bkt2", "bkt3", "rucket-11"},
				},
				{
					name: "retrieves packages from raw single and list",
					reqBody: pkger.ReqApply{
						DryRun:      true,
						OrgID:       platform.ID(9000).String(),
						RawTemplate: newBktPkg(t, "bkt4"),
						RawTemplates: []pkger.ReqRawTemplate{
							newBktPkg(t, "bkt1"),
							newBktPkg(t, "bkt2"),
							newBktPkg(t, "bkt3"),
						},
					},
					expectedBkts: []string{"bkt1", "bkt2", "bkt3", "bkt4"},
				},
			}

			for _, tt := range tests {
				fn := func(t *testing.T) {
					svc := &fakeSVC{
						dryRunFn: func(ctx context.Context, orgID, userID platform.ID, opts ...pkger.ApplyOptFn) (pkger.ImpactSummary, error) {
							var opt pkger.ApplyOpt
							for _, o := range opts {
								o(&opt)
							}
							pkg, err := pkger.Combine(opt.Templates)
							if err != nil {
								return pkger.ImpactSummary{}, err
							}

							if err := pkg.Validate(); err != nil {
								return pkger.ImpactSummary{}, err
							}
							sum := pkg.Summary()
							var diff pkger.Diff
							for _, b := range sum.Buckets {
								diff.Buckets = append(diff.Buckets, pkger.DiffBucket{
									DiffIdentifier: pkger.DiffIdentifier{
										MetaName: b.Name,
									},
								})
							}

							return pkger.ImpactSummary{
								Diff:    diff,
								Summary: sum,
							}, nil
						},
					}

					pkgHandler := pkger.NewHTTPServerTemplates(zap.NewNop(), svc, defaultClient)
					svr := newMountedHandler(pkgHandler, 1)

					testttp.
						PostJSON(t, "/api/v2/templates/apply", tt.reqBody).
						Do(svr).
						ExpectStatus(http.StatusOK).
						ExpectBody(func(buf *bytes.Buffer) {
							var resp pkger.RespApply
							decodeBody(t, buf, &resp)

							require.Len(t, resp.Summary.Buckets, len(tt.expectedBkts))
							for i, expected := range tt.expectedBkts {
								assert.Equal(t, expected, resp.Summary.Buckets[i].Name)
							}
						})
				}

				t.Run(tt.name, fn)
			}
		})

		t.Run("validation failures", func(t *testing.T) {
			tests := []struct {
				name               string
				contentType        string
				reqBody            pkger.ReqApply
				expectedStatusCode int
			}{
				{
					name:        "invalid org id",
					contentType: "application/json",
					reqBody: pkger.ReqApply{
						DryRun:      true,
						OrgID:       "bad org id",
						RawTemplate: bucketPkgKinds(t, pkger.EncodingJSON),
					},
					expectedStatusCode: http.StatusBadRequest,
				},
				{
					name:        "invalid stack id",
					contentType: "application/json",
					reqBody: pkger.ReqApply{
						DryRun:      true,
						OrgID:       platform.ID(9000).String(),
						StackID:     strPtr("invalid stack id"),
						RawTemplate: bucketPkgKinds(t, pkger.EncodingJSON),
					},
					expectedStatusCode: http.StatusBadRequest,
				},
			}

			for _, tt := range tests {
				fn := func(t *testing.T) {
					svc := &fakeSVC{
						dryRunFn: func(ctx context.Context, orgID, userID platform.ID, opts ...pkger.ApplyOptFn) (pkger.ImpactSummary, error) {
							var opt pkger.ApplyOpt
							for _, o := range opts {
								o(&opt)
							}
							pkg, err := pkger.Combine(opt.Templates)
							if err != nil {
								return pkger.ImpactSummary{}, err
							}
							return pkger.ImpactSummary{
								Summary: pkg.Summary(),
							}, nil
						},
					}

					pkgHandler := pkger.NewHTTPServerTemplates(zap.NewNop(), svc, defaultClient)
					svr := newMountedHandler(pkgHandler, 1)

					testttp.
						PostJSON(t, "/api/v2/templates/apply", tt.reqBody).
						Headers("Content-Type", tt.contentType).
						Do(svr).
						ExpectStatus(tt.expectedStatusCode)
				}

				t.Run(tt.name, fn)
			}
		})

		t.Run("resp apply err response", func(t *testing.T) {
			tests := []struct {
				name        string
				contentType string
				reqBody     pkger.ReqApply
			}{
				{
					name:        "invalid json",
					contentType: "application/json",
					reqBody: pkger.ReqApply{
						DryRun:      true,
						OrgID:       platform.ID(9000).String(),
						RawTemplate: simpleInvalidBody(t, pkger.EncodingJSON),
					},
				},
			}
			for _, tt := range tests {
				fn := func(t *testing.T) {
					svc := &fakeSVC{
						dryRunFn: func(ctx context.Context, orgID, userID platform.ID, opts ...pkger.ApplyOptFn) (pkger.ImpactSummary, error) {
							var opt pkger.ApplyOpt
							for _, o := range opts {
								o(&opt)
							}
							pkg, err := pkger.Combine(opt.Templates)
							if err != nil {
								return pkger.ImpactSummary{}, err
							}

							if err := pkg.Validate(); err != nil {
								return pkger.ImpactSummary{}, err
							}
							sum := pkg.Summary()
							var diff pkger.Diff
							for _, b := range sum.Buckets {
								diff.Buckets = append(diff.Buckets, pkger.DiffBucket{
									DiffIdentifier: pkger.DiffIdentifier{
										MetaName: b.Name,
									},
								})
							}
							return pkger.ImpactSummary{
								Summary: sum,
								Diff:    diff,
							}, nil
						},
					}

					pkgHandler := pkger.NewHTTPServerTemplates(zap.NewNop(), svc, defaultClient)
					svr := newMountedHandler(pkgHandler, 1)

					testttp.
						PostJSON(t, "/api/v2/templates/apply", tt.reqBody).
						Headers("Content-Type", tt.contentType).
						Do(svr).
						ExpectStatus(http.StatusUnprocessableEntity).
						ExpectBody(func(buf *bytes.Buffer) {
							var resp pkger.RespApplyErr
							decodeBody(t, buf, &resp)
							require.Equal(t, "unprocessable entity", resp.Code)
							require.Greater(t, len(resp.Message), 0)
							require.NotNil(t, resp.Summary)
							require.NotNil(t, resp.Diff)
							require.Greater(t, len(resp.Errors), 0)
						})
				}
				t.Run(tt.name, fn)
			}
		})
	})

	t.Run("apply a pkg", func(t *testing.T) {
		t.Run("happy path", func(t *testing.T) {
			svc := &fakeSVC{
				applyFn: func(ctx context.Context, orgID, userID platform.ID, opts ...pkger.ApplyOptFn) (pkger.ImpactSummary, error) {
					var opt pkger.ApplyOpt
					for _, o := range opts {
						o(&opt)
					}

					pkg, err := pkger.Combine(opt.Templates)
					if err != nil {
						return pkger.ImpactSummary{}, err
					}

					sum := pkg.Summary()

					var diff pkger.Diff
					for _, b := range sum.Buckets {
						diff.Buckets = append(diff.Buckets, pkger.DiffBucket{
							DiffIdentifier: pkger.DiffIdentifier{
								MetaName: b.Name,
							},
						})
					}
					for key := range opt.MissingSecrets {
						sum.MissingSecrets = append(sum.MissingSecrets, key)
					}

					return pkger.ImpactSummary{
						Diff:    diff,
						Summary: sum,
					}, nil
				},
			}

			pkgHandler := pkger.NewHTTPServerTemplates(zap.NewNop(), svc, defaultClient)
			svr := newMountedHandler(pkgHandler, 1)

			testttp.
				PostJSON(t, "/api/v2/templates/apply", pkger.ReqApply{
					OrgID:       platform.ID(9000).String(),
					Secrets:     map[string]string{"secret1": "val1"},
					RawTemplate: bucketPkgKinds(t, pkger.EncodingJSON),
				}).
				Do(svr).
				ExpectStatus(http.StatusCreated).
				ExpectBody(func(buf *bytes.Buffer) {
					var resp pkger.RespApply
					decodeBody(t, buf, &resp)

					assert.Len(t, resp.Summary.Buckets, 1)
					assert.Len(t, resp.Diff.Buckets, 1)
					assert.Equal(t, []string{"secret1"}, resp.Summary.MissingSecrets)
					assert.Nil(t, resp.Errors)
				})
		})

		t.Run("all diff and summary resource collections are non null", func(t *testing.T) {
			svc := &fakeSVC{
				applyFn: func(ctx context.Context, orgID, userID platform.ID, opts ...pkger.ApplyOptFn) (pkger.ImpactSummary, error) {
					// return zero value pkg
					return pkger.ImpactSummary{}, nil
				},
			}

			pkgHandler := pkger.NewHTTPServerTemplates(zap.NewNop(), svc, defaultClient)
			svr := newMountedHandler(pkgHandler, 1)

			testttp.
				PostJSON(t, "/api/v2/templates/apply", pkger.ReqApply{
					OrgID:       platform.ID(1).String(),
					RawTemplate: bucketPkgKinds(t, pkger.EncodingJSON),
				}).
				Headers("Content-Type", "application/json").
				Do(svr).
				ExpectStatus(http.StatusCreated).
				ExpectBody(func(buf *bytes.Buffer) {
					var resp pkger.RespApply
					decodeBody(t, buf, &resp)
					assertNonZeroApplyResp(t, resp)
				})
		})
	})

	t.Run("Templates()", func(t *testing.T) {
		tests := []struct {
			name     string
			reqBody  pkger.ReqApply
			encoding pkger.Encoding
		}{
			{
				name: "jsonnet disabled",
				reqBody: pkger.ReqApply{
					OrgID:       platform.ID(9000).String(),
					RawTemplate: bucketPkgKinds(t, pkger.EncodingJsonnet),
				},
				encoding: pkger.EncodingJsonnet,
			},
			{
				name: "jsonnet remote disabled",
				reqBody: pkger.ReqApply{
					OrgID: platform.ID(9000).String(),
					Remotes: []pkger.ReqTemplateRemote{{
						URL: newPkgURL(t, filesvr.URL, "testdata/bucket_associates_labels.jsonnet"),
					}},
				},
				encoding: pkger.EncodingJsonnet,
			},
			{
				name: "jsonnet disabled remote source",
				reqBody: pkger.ReqApply{
					OrgID:       platform.ID(9000).String(),
					RawTemplate: bucketPkgJsonWithJsonnetRemote(t),
				},
				encoding: pkger.EncodingJSON,
			},
		}

		for _, tt := range tests {
			tmpl, err := tt.reqBody.Templates(tt.encoding, defaultClient)
			assert.Nil(t, tmpl)
			require.Error(t, err)
			assert.Equal(t, "unprocessable entity", influxerror.ErrorCode(err))
			assert.Contains(t, influxerror.ErrorMessage(err), "invalid encoding provided: jsonnet")
		}
	})

	t.Run("Templates() remotes with IP validation", func(t *testing.T) {
		tests := []struct {
			name    string
			client  *http.Client
			expCode int
			expErr  string
		}{
			{
				name:    "no filter ip",
				client:  pkger.NewDefaultHTTPClient(fluxurl.PassValidator{}),
				expCode: http.StatusOK,
				expErr:  "",
			},
			{
				name:    "filter ip",
				client:  pkger.NewDefaultHTTPClient(fluxurl.PrivateIPValidator{}),
				expCode: http.StatusUnprocessableEntity,
				expErr:  "no such host",
			},
		}

		svc := &fakeSVC{
			dryRunFn: func(ctx context.Context, orgID, userID platform.ID, opts ...pkger.ApplyOptFn) (pkger.ImpactSummary, error) {
				return pkger.ImpactSummary{}, nil
			},
		}
		for _, tt := range tests {
			core, sink := observer.New(zap.InfoLevel)
			pkgHandler := pkger.NewHTTPServerTemplates(zap.New(core), svc, tt.client)
			svr := newMountedHandler(pkgHandler, 1)

			reqBody := pkger.ReqApply{
				DryRun: true,
				OrgID:  platform.ID(9000).String(),
				Remotes: []pkger.ReqTemplateRemote{{
					URL: newPkgURL(t, filesvr.URL, "testdata/remote_bucket.json"),
				}},
			}

			ctx := context.Background()
			testttp.
				PostJSON(t, "/api/v2/templates/apply", reqBody).
				Headers("Content-Type", "application/json").
				WithCtx(ctx).
				Do(svr).
				ExpectStatus(tt.expCode).
				ExpectBody(func(buf *bytes.Buffer) {
					var resp pkger.RespApply
					decodeBody(t, buf, &resp)

					assert.Len(t, resp.Summary.Buckets, 0)
					assert.Len(t, resp.Diff.Buckets, 0)
				})

			if tt.expErr != "" {
				// Verify logging output has the expected generic flux message
				entries := sink.TakeAll() // resets to 0
				fmt.Printf("%+v\n", entries)
				require.Equal(t, 1, len(entries))
				assert.Contains(t, fmt.Sprintf("%s", entries[0].Context[0].Interface), tt.expErr)
			}
		}
	})
}

func assertNonZeroApplyResp(t *testing.T, resp pkger.RespApply) {
	t.Helper()

	assert.NotNil(t, resp.Sources)

	assert.NotNil(t, resp.Diff.Buckets)
	assert.NotNil(t, resp.Diff.Checks)
	assert.NotNil(t, resp.Diff.Dashboards)
	assert.NotNil(t, resp.Diff.Labels)
	assert.NotNil(t, resp.Diff.LabelMappings)
	assert.NotNil(t, resp.Diff.NotificationEndpoints)
	assert.NotNil(t, resp.Diff.NotificationRules)
	assert.NotNil(t, resp.Diff.Tasks)
	assert.NotNil(t, resp.Diff.Telegrafs)
	assert.NotNil(t, resp.Diff.Variables)

	assert.NotNil(t, resp.Summary.Buckets)
	assert.NotNil(t, resp.Summary.Checks)
	assert.NotNil(t, resp.Summary.Dashboards)
	assert.NotNil(t, resp.Summary.Labels)
	assert.NotNil(t, resp.Summary.LabelMappings)
	assert.NotNil(t, resp.Summary.NotificationEndpoints)
	assert.NotNil(t, resp.Summary.NotificationRules)
	assert.NotNil(t, resp.Summary.Tasks)
	assert.NotNil(t, resp.Summary.TelegrafConfigs)
	assert.NotNil(t, resp.Summary.Variables)
}

func bucketPkgKinds(t *testing.T, encoding pkger.Encoding) pkger.ReqRawTemplate {
	t.Helper()

	var pkgStr string
	switch encoding {
	case pkger.EncodingJsonnet:
		pkgStr = `
local Bucket(name, desc) = {
    apiVersion: '%[1]s',
    kind: 'Bucket',
    metadata: {
        name: name
    },
    spec: {
        description: desc
    }
};

[
  Bucket(name="rucket-1", desc="bucket 1 description"),
]
`
	case pkger.EncodingJSON:
		pkgStr = `[
  {
    "apiVersion": "%[1]s",
    "kind": "Bucket",
    "metadata": {
      "name": "rucket-11"
    },
    "spec": {
      "description": "bucket 1 description"
    }
  }
]
`
	case pkger.EncodingYAML:
		pkgStr = `apiVersion: %[1]s
kind: Bucket
metadata:
  name:  rucket-11
spec:
  description: bucket 1 description
`
	default:
		require.FailNow(t, "invalid encoding provided: "+encoding.String())
	}

	pkg, err := pkger.Parse(encoding, pkger.FromString(fmt.Sprintf(pkgStr, pkger.APIVersion)), pkger.EnableJsonnet())
	require.NoError(t, err)

	b, err := pkg.Encode(encoding)
	require.NoError(t, err)
	return pkger.ReqRawTemplate{
		ContentType: encoding.String(),
		Sources:     pkg.Sources(),
		Template:    b,
	}
}

func bucketPkgJsonWithJsonnetRemote(t *testing.T) pkger.ReqRawTemplate {
	pkgStr := `[
  {
    "apiVersion": "%[1]s",
    "kind": "Bucket",
    "metadata": {
      "name": "rucket-11"
    },
    "spec": {
      "description": "bucket 1 description"
    }
  }
]
`
	// Create a json template and then add a jsonnet remote raw template
	pkg, err := pkger.Parse(pkger.EncodingJSON, pkger.FromString(fmt.Sprintf(pkgStr, pkger.APIVersion)))
	require.NoError(t, err)

	b, err := pkg.Encode(pkger.EncodingJSON)
	require.NoError(t, err)
	return pkger.ReqRawTemplate{
		ContentType: pkger.EncodingJsonnet.String(),
		Sources:     []string{"file:///nonexistent.jsonnet"},
		Template:    b,
	}
}

func simpleInvalidBody(t *testing.T, encoding pkger.Encoding) pkger.ReqRawTemplate {
	t.Helper()
	b := bytes.Buffer{}
	b.WriteString("[ {}, {} ]")
	return pkger.ReqRawTemplate{
		ContentType: encoding.String(),
		Sources:     []string{"test1.json"},
		Template:    b.Bytes(),
	}
}

func newReqApplyYMLBody(t *testing.T, orgID platform.ID, dryRun bool) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer
	err := yaml.NewEncoder(&buf).Encode(pkger.ReqApply{
		DryRun:      dryRun,
		OrgID:       orgID.String(),
		RawTemplate: bucketPkgKinds(t, pkger.EncodingYAML),
	})
	require.NoError(t, err)
	return &buf
}

func decodeBody(t *testing.T, r io.Reader, v interface{}) {
	t.Helper()

	if err := json.NewDecoder(r).Decode(v); err != nil {
		require.FailNow(t, err.Error())
	}
}

func newMountedHandler(rh kithttp.ResourceHandler, userID platform.ID) chi.Router {
	r := chi.NewRouter()
	r.Mount(rh.Prefix(), authMW(userID)(rh))
	return r
}

func authMW(userID platform.ID) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		fn := func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(pcontext.SetAuthorizer(r.Context(), &influxdb.Session{UserID: userID}))
			next.ServeHTTP(w, r)
		}
		return http.HandlerFunc(fn)
	}
}
