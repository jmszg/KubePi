package cluster

import (
	"fmt"
	"github.com/KubeOperator/ekko/internal/api/v1/session"
	v1 "github.com/KubeOperator/ekko/internal/model/v1"
	v1Cluster "github.com/KubeOperator/ekko/internal/model/v1/cluster"
	"github.com/KubeOperator/ekko/internal/server"
	"github.com/KubeOperator/ekko/internal/service/v1/cluster"
	"github.com/KubeOperator/ekko/internal/service/v1/clusterbinding"
	"github.com/KubeOperator/ekko/internal/service/v1/common"
	pkgV1 "github.com/KubeOperator/ekko/pkg/api/v1"
	"github.com/KubeOperator/ekko/pkg/certificate"
	"github.com/KubeOperator/ekko/pkg/kubernetes"
	"github.com/asdine/storm/v3"
	"github.com/kataras/iris/v12"
	"github.com/kataras/iris/v12/context"
	authV1 "k8s.io/api/authorization/v1"
	"sync"
)

type Handler struct {
	clusterService        cluster.Service
	clusterBindingService clusterbinding.Service
}

func NewHandler() *Handler {
	return &Handler{
		clusterService:        cluster.NewService(),
		clusterBindingService: clusterbinding.NewService(),
	}
}

func (h *Handler) CreateCluster() iris.Handler {
	return func(ctx *context.Context) {
		var req Cluster
		if err := ctx.ReadJSON(&req); err != nil {
			ctx.StatusCode(iris.StatusBadRequest)
			ctx.Values().Set("message", err.Error())
			return
		}
		if req.ConfigFileContentStr != "" {
			req.Spec.Authentication.ConfigFileContent = []byte(req.ConfigFileContentStr)
		}
		if req.CaDataStr != "" {
			req.CaCertificate.CertData = []byte(req.CaDataStr)
		}
		if req.Spec.Authentication.Mode == "certificate" {
			req.Spec.Authentication.Certificate.CertData = []byte(req.CertDataStr)
			req.Spec.Authentication.Certificate.KeyData = []byte(req.KeyDataStr)
		}
		// 生成一个rsa格式的私钥
		privateKey, err := certificate.GeneratePrivateKey()
		if err != nil {
			ctx.StatusCode(iris.StatusInternalServerError)
			ctx.Values().Set("message", err.Error())
			return

		}
		req.PrivateKey = privateKey
		client := kubernetes.NewKubernetes(&req.Cluster)
		if err := client.Ping(); err != nil {
			ctx.StatusCode(iris.StatusInternalServerError)
			ctx.Values().Set("message", err.Error())
			return
		}
		v, _ := client.Version()
		req.Status.Version = v.GitVersion
		if req.Spec.Authentication.Mode == "configFile" {
			kubeCfg, err := client.Config()
			if err != nil {
				ctx.StatusCode(iris.StatusInternalServerError)
				ctx.Values().Set("message", err.Error())
				return
			}
			req.Spec.Connect.Forward.ApiServer = kubeCfg.Host
		}
		u := ctx.Values().Get("profile")
		profile := u.(session.UserProfile)
		req.CreatedBy = profile.Name

		tx, err := server.DB().Begin(true)
		if err != nil {
			ctx.StatusCode(iris.StatusInternalServerError)
			ctx.Values().Set("message", err.Error())
			return
		}
		txOptions := common.DBOptions{DB: tx}
		req.Cluster.Status.Phase = clusterStatusSaved
		if err := h.clusterService.Create(&req.Cluster, txOptions); err != nil {
			ctx.StatusCode(iris.StatusInternalServerError)
			ctx.Values().Set("message", err.Error())
			return
		}
		requiredPermissions := map[string][]string{
			"namespaces":       {"get", "post", "delete"},
			"clusterroles":     {"get", "post", "delete"},
			"clusterrolebings": {"get", "post", "delete"},
			"roles":            {"get", "post", "delete"},
			"rolebindings":     {"get", "post", "delete"},
		}
		notAllowed, err := checkRequiredPermissions(client, requiredPermissions)
		if err != nil {
			ctx.StatusCode(iris.StatusInternalServerError)
			ctx.Values().Set("message", err.Error())
			return
		}
		if notAllowed != "" {
			ctx.StatusCode(iris.StatusInternalServerError)
			ctx.Values().Set("message", []string{"permission %s required", notAllowed})
			return
		}
		if err := client.CreateOrUpdateClusterRoleBinding("admin-cluster", profile.Name, true); err != nil {
			_ = tx.Rollback()
			ctx.StatusCode(iris.StatusInternalServerError)
			ctx.Values().Set("message", err.Error())
			return
		}
		_ = tx.Commit()
		go func() {
			req.Status.Phase = clusterStatusInitializing
			if e := h.clusterService.Update(req.Name, &req.Cluster, common.DBOptions{}); e != nil {
				server.Logger().Errorf("cna not update cluster status %s", err)
				return
			}
			if err := client.CreateDefaultClusterRoles(); err != nil {
				req.Status.Phase = clusterStatusFailed
				req.Status.Message = err.Error()
				if e := h.clusterService.Update(req.Name, &req.Cluster, common.DBOptions{}); e != nil {
					server.Logger().Errorf("cna not update cluster status %s", err)
					return
				}
				server.Logger().Errorf("cna not init  built in clusterroles %s", err)
				return
			}
			if err := h.createClusterUser(client, profile.Name, req.Name); err != nil {
				req.Status.Phase = clusterStatusFailed
				req.Status.Message = err.Error()
				if e := h.clusterService.Update(req.Name, &req.Cluster, common.DBOptions{}); e != nil {
					server.Logger().Errorf("cna not update cluster status %s", err)
					return
				}
				server.Logger().Errorf("cna not create cluster user  %s", err)
				return
			}
			req.Status.Phase = clusterStatusCompleted
			if e := h.clusterService.Update(req.Name, &req.Cluster, common.DBOptions{}); e != nil {
				server.Logger().Errorf("cna not update cluster status %s", err)
				return
			}
		}()
		ctx.Values().Set("data", &req)
	}
}

func (h *Handler) createClusterUser(client kubernetes.Interface, username string, clusterName string) error {
	binding := v1Cluster.Binding{
		BaseModel: v1.BaseModel{
			Kind: "ClusterBinding",
		},
		Metadata: v1.Metadata{
			Name: fmt.Sprintf("%s-%s-cluster-binding", clusterName, username),
		},
		UserRef:    username,
		ClusterRef: clusterName,
	}
	csr, err := client.CreateCommonUser(username)
	if err != nil {
		return err
	}
	binding.Certificate = csr
	if err := h.clusterBindingService.CreateClusterBinding(&binding, common.DBOptions{}); err != nil {
		return err
	}
	return nil
}

func checkRequiredPermissions(client kubernetes.Interface, requiredPermissions map[string][]string) (string, error) {
	wg := sync.WaitGroup{}
	errCh := make(chan error)
	resultCh := make(chan kubernetes.PermissionCheckResult)
	doneCh := make(chan struct{})
	for key := range requiredPermissions {
		for i := range requiredPermissions[key] {
			wg.Add(1)
			i := i
			go func(key string, index int) {
				rs, err := client.HasPermission(authV1.ResourceAttributes{
					Verb:     requiredPermissions[key][i],
					Resource: key,
				})
				if err != nil {
					errCh <- err
					wg.Done()
					return
				}
				resultCh <- rs
				wg.Done()
			}(key, i)
		}
	}
	go func() {
		wg.Wait()
		doneCh <- struct{}{}
	}()
	for {
		select {
		case <-doneCh:
			goto end
		case err := <-errCh:
			return "", err
		case b := <-resultCh:
			if !b.Allowed {
				return fmt.Sprintf("%s-%s"), nil
			}
		}
	}
end:
	return "", nil
}

func (h *Handler) SearchClusters() iris.Handler {
	return func(ctx *context.Context) {
		pageNum, _ := ctx.Values().GetInt(pkgV1.PageNum)
		pageSize, _ := ctx.Values().GetInt(pkgV1.PageSize)
		var conditions pkgV1.Conditions
		if ctx.GetContentLength() > 0 {
			if err := ctx.ReadJSON(&conditions); err != nil {
				ctx.StatusCode(iris.StatusBadRequest)
				ctx.Values().Set("message", err.Error())
				return
			}
		}
		clusters, total, err := h.clusterService.Search(pageNum, pageSize, conditions, common.DBOptions{})
		if err != nil && err != storm.ErrNotFound {
			ctx.StatusCode(iris.StatusInternalServerError)
			ctx.Values().Set("message", err.Error())
			return
		}
		ctx.Values().Set("data", pkgV1.Page{Items: clusters, Total: total})
	}
}
func (h *Handler) GetCluster() iris.Handler {
	return func(ctx *context.Context) {
		name := ctx.Params().GetString("name")
		c, err := h.clusterService.Get(name, common.DBOptions{})
		if err != nil {
			ctx.StatusCode(iris.StatusBadRequest)
			ctx.Values().Set("message", fmt.Sprintf("get clusters failed: %s", err.Error()))
			return
		}
		ctx.Values().Set("data", c)
	}
}

func (h *Handler) ListClusters() iris.Handler {
	return func(ctx *context.Context) {
		var clusters []v1Cluster.Cluster
		clusters, err := h.clusterService.List(common.DBOptions{})
		if err != nil {
			ctx.StatusCode(iris.StatusBadRequest)
			ctx.Values().Set("message", fmt.Sprintf("get clusters failed: %s", err.Error()))
			return
		}
		var resultClusters []Cluster
		u := ctx.Values().Get("profile")
		profile := u.(session.UserProfile)
		for i := range clusters {
			mbs, err := h.clusterBindingService.GetClusterBindingByClusterName(clusters[i].Name, common.DBOptions{})
			if err != nil {
				ctx.StatusCode(iris.StatusBadRequest)
				ctx.Values().Set("message", err)
				return
			}
			rc := Cluster{
				Cluster: clusters[i],
			}
			for j := range mbs {
				if mbs[j].UserRef == profile.Name {
					rc.Accessable = true
				}
			}
			resultClusters = append(resultClusters, rc)
		}
		ctx.StatusCode(iris.StatusOK)
		ctx.Values().Set("data", resultClusters)
	}
}

func (h *Handler) DeleteCluster() iris.Handler {
	return func(ctx *context.Context) {
		name := ctx.Params().GetString("name")
		c, err := h.clusterService.Get(name, common.DBOptions{})
		if err != nil {
			ctx.StatusCode(iris.StatusInternalServerError)
			ctx.Values().Set("message", fmt.Sprintf("get cluster failed: %s", err.Error()))
			return
		}
		tx, err := server.DB().Begin(true)
		if err != nil {
			ctx.StatusCode(iris.StatusInternalServerError)
			ctx.Values().Set("message", fmt.Sprintf("delete cluster failed: %s", err.Error()))
			return
		}
		txOptions := common.DBOptions{DB: tx}

		if err := h.clusterService.Delete(name, txOptions); err != nil {
			_ = tx.Rollback()
			ctx.StatusCode(iris.StatusBadRequest)
			ctx.Values().Set("message", fmt.Sprintf("delete cluster failed: %s", err.Error()))
			return
		}
		clusterBindings, err := h.clusterBindingService.GetClusterBindingByClusterName(name, txOptions)
		if err != nil {
			_ = tx.Rollback()
			ctx.StatusCode(iris.StatusInternalServerError)
			ctx.Values().Set("message", fmt.Sprintf("delete cluster failed: %s", err.Error()))
			return
		}
		for i := range clusterBindings {
			if err := h.clusterBindingService.Delete(clusterBindings[i].Name, txOptions); err != nil {
				_ = tx.Rollback()
				ctx.StatusCode(iris.StatusInternalServerError)
				ctx.Values().Set("message", fmt.Sprintf("delete cluster failed: %s", err.Error()))
				return
			}
		}
		k := kubernetes.NewKubernetes(c)
		if err := k.CleanAllRBACResource(); err != nil {
			_ = tx.Rollback()
			ctx.StatusCode(iris.StatusInternalServerError)
			ctx.Values().Set("message", err)
			return
		}
		_ = tx.Commit()
		ctx.StatusCode(iris.StatusOK)
	}
}

func (h *Handler) Privilege() iris.Handler {
	return func(ctx *context.Context) {
		var req Privilege
		if err := ctx.ReadJSON(&req); err != nil {
			ctx.StatusCode(iris.StatusBadRequest)
			ctx.Values().Set("message", err)
			return
		}
		ctx.Redirect(req.Url)
		return
	}
}

func Install(parent iris.Party) {
	handler := NewHandler()
	sp := parent.Party("/clusters")
	sp.Post("", handler.CreateCluster())
	sp.Get("", handler.ListClusters())
	sp.Get("/:name", handler.GetCluster())
	sp.Delete("/:name", handler.DeleteCluster())
	sp.Post("/search", handler.SearchClusters())
	sp.Get("/:name/members", handler.ListClusterMembers())
	sp.Post("/:name/members", handler.CreateClusterMember())
	sp.Delete("/:name/members/:member", handler.DeleteClusterMember())
	sp.Put("/:name/members/:member", handler.UpdateClusterMember())
	sp.Get("/:name/members/:member", handler.GetClusterMember())
	sp.Get("/:name/clusterroles", handler.ListClusterRoles())
	sp.Post("/:name/clusterroles", handler.CreateClusterRole())
	sp.Put("/:name/clusterroles/:clusterrole", handler.UpdateClusterRole())
	sp.Delete("/:name/clusterroles/:clusterrole", handler.DeleteClusterRole())
	sp.Get("/:name/apigroups", handler.ListApiGroups())
	sp.Get("/:name/apigroups/{group:path}", handler.ListApiGroupResources())
	sp.Get("/:name/namespaces", handler.ListNamespace())
	sp.Get("/:name/terminal/session", handler.TerminalSessionHandler())
	sp.Get("/:name/logging/session", handler.LoggingHandler())
	sp.Post("/privilege", handler.Privilege())
}
