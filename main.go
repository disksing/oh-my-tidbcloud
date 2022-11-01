package main

import (
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	apiclient "github.com/c4pt0r/go-tidbcloud-sdk-v1/client"
	"github.com/c4pt0r/go-tidbcloud-sdk-v1/client/cluster"
	"github.com/c4pt0r/go-tidbcloud-sdk-v1/client/project"
	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	httptransport "github.com/go-openapi/runtime/client"
	"github.com/go-openapi/strfmt"
	"github.com/icholy/digest"
	"go.uber.org/zap"
)

var (
	publicKeys  []string
	privateKeys []string
)

func init() {
	for i := 1; ; i++ {
		if pub := os.Getenv(fmt.Sprintf("PUBKEY%d", i)); pub != "" {
			publicKeys = append(publicKeys, pub)
			privateKeys = append(privateKeys, os.Getenv(fmt.Sprintf("PRIKEY%d", i)))
		} else {
			break
		}
	}
}

var (
	region  = flag.String("region", "", "region to use, default random")
	threads = flag.Int("threads", 1, "number of threads")
	record  = flag.Bool("record", false, "record data to csv, only work when thread=1")
)

type Context struct {
	Logger       logr.Logger
	Client       *apiclient.GoTidbcloud
	Region       string
	Project      string
	Measurements map[string]float32
}

func selectRegion(ctx *Context) error {
	if *region != "" {
		ctx.Region = *region
		return nil
	}

	artifacts, err := ctx.Client.Cluster.ListProviderRegions(nil)
	if err != nil {
		return err
	}
	var regions []string
	payload := artifacts.GetPayload()
	for _, item := range payload.Items {
		if item.ClusterType == "DEVELOPER" {
			regions = append(regions, item.Region)
		}
	}
	ctx.Logger.Info("listed regions", "regions", regions)

	ctx.Region = regions[rand.Intn(len(regions))]

	ctx.Logger.Info("select region", "region", ctx.Region)

	return nil
}

func selectProject(ctx *Context) error {
	projects, err := ctx.Client.Project.ListProjects(project.NewListProjectsParams())
	if err != nil {
		return err
	}
	var projectIDs []string
	payload := projects.GetPayload()
	for _, item := range payload.Items {
		projectIDs = append(projectIDs, item.ID)
	}
	ctx.Logger.Info("listed projects", "projects", projectIDs)
	ctx.Project = projectIDs[rand.Intn(len(projectIDs))]
	ctx.Logger.Info("select project", "project", ctx.Project)
	return nil
}

func deleteAllCluster(ctx *Context) error {
	projects, err := ctx.Client.Project.ListProjects(project.NewListProjectsParams())
	if err != nil {
		return err
	}
	for _, p := range projects.Payload.Items {
		if p.ClusterCount > 0 {
			ctx.Logger.Info("deleting cluster", "project", p.ID)

			clusters, err := ctx.Client.Cluster.ListClustersOfProject(cluster.NewListClustersOfProjectParams().WithProjectID(p.ID))
			if err != nil {
				return err
			}
			for _, c := range clusters.Payload.Items {
				_, err := ctx.Client.Cluster.DeleteCluster(cluster.NewDeleteClusterParams().WithProjectID(p.ID).WithClusterID(*c.ID))
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func stringP(s string) *string {
	return &s
}

func createCluster(ctx *Context) error {
	ctx.Logger.Info("creating cluster")

	createClusterBody := cluster.CreateClusterBody{
		Name:          stringP(fmt.Sprintf("t%v", time.Now().Unix())),
		CloudProvider: stringP("AWS"),
		ClusterType:   stringP("DEVELOPER"),
		Region:        stringP(ctx.Region),
		Config: &cluster.CreateClusterParamsBodyConfig{
			RootPassword: stringP("00000000"),
		},
	}

	createClusterResult, err := ctx.Client.Cluster.CreateCluster(cluster.NewCreateClusterParams().
		WithProjectID(ctx.Project).
		WithBody(createClusterBody))
	if err != nil {
		return err
	}

	for {
		clusterResult, err := ctx.Client.Cluster.GetCluster(cluster.NewGetClusterParams().WithClusterID(*createClusterResult.Payload.ID).WithProjectID(ctx.Project))
		if err != nil {
			return err
		}
		s := clusterResult.GetPayload().Status.ClusterStatus
		if s == "AVAILABLE" {
			break
		}
		time.Sleep(time.Second)
	}
	return nil
}

func OneByOne(f ...func(*Context) error) func(*Context) error {
	return func(ctx *Context) error {
		for _, fn := range f {
			if err := fn(ctx); err != nil {
				return err
			}
		}
		return nil
	}
}

func Measure(name string, f func(*Context) error) func(*Context) error {
	return func(ctx *Context) error {
		start := time.Now()
		err := f(ctx)
		elapsed := time.Since(start)
		seconds, _ := strconv.ParseFloat(fmt.Sprintf("%.2f", elapsed.Seconds()), 32)
		ctx.Logger.Info("measure", "name", name, "elapsed", seconds)
		ctx.Measurements[name] = float32(seconds)
		return err
	}
}

var prepare = OneByOne(selectRegion, selectProject, deleteAllCluster)

func measureCreateCluster(ctx *Context) error {
	err := OneByOne(
		prepare,
		Measure("create cluster", createCluster),
	)(ctx)
	if err != nil {
		return err
	}
	return nil
}

func recordCSV(fields ...string) error {
	f, err := os.OpenFile("stats.csv", os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(strings.Join(fields, ",") + "\n")
	return err
}

func main() {
	flag.Parse()
	now := time.Now()

	var log logr.Logger
	zaplog, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}
	log = zapr.NewLogger(zaplog)

	log.Info("initializing with keys", "public", publicKeys)

	if len(publicKeys) == 0 {
		log.Info("no keys found, please set env PUBKEY1, PRIMKEY1")
		return
	}
	if len(publicKeys) < *threads {
		log.Info("not enough keys", "keys", len(publicKeys), "threads", *threads)
		return
	}
	if *threads <= 0 {
		log.Info("invalid thread (need >=1)")
		return
	}

	var clients []*apiclient.GoTidbcloud
	for i := range publicKeys {
		clients = append(clients, apiclient.New(
			httptransport.NewWithClient("api.tidbcloud.com", "/", []string{"https"}, &http.Client{
				Transport: &digest.Transport{
					Username: publicKeys[i],
					Password: privateKeys[i],
				},
			}),
			strfmt.Default))
	}

	if *threads == 1 {
		ctx := &Context{
			Client:       clients[0],
			Logger:       log.WithValues("index", 0).WithValues("key", publicKeys[0]),
			Measurements: make(map[string]float32),
		}
		err := measureCreateCluster(ctx)
		if err != nil {
			log.Error(err, "failed to create cluster")
		}

		if *record {
			var err2 error
			if err != nil {
				err2 = recordCSV(now.Format("2006-01-02 15:04:05"), ctx.Region, "N", "")
			} else {
				err2 = recordCSV(now.Format("2006-01-02 15:04:05"), ctx.Region, "Y", fmt.Sprintf("%.2f", ctx.Measurements["create cluster"]))
			}
			if err2 != nil {
				log.Error(err2, "failed to record csv")
			}
		}
		return
	}

	var wg sync.WaitGroup
	wg.Add(*threads)
	for i := 0; i < *threads; i++ {
		ctx := &Context{
			Client:       clients[i],
			Logger:       log.WithValues("index", i).WithValues("key", publicKeys[i]),
			Measurements: make(map[string]float32),
		}
		go func() {
			defer wg.Done()

			if err := measureCreateCluster(ctx); err != nil {
				log.Error(err, "failed to create cluster")
				return
			}
		}()
	}
	wg.Wait()
}
