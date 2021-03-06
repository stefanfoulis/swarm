package api

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"runtime"
	"sort"
	"strings"

	log "github.com/Sirupsen/logrus"
	dockerfilters "github.com/docker/docker/pkg/parsers/filters"
	"github.com/docker/swarm/cluster"
	"github.com/docker/swarm/scheduler"
	"github.com/docker/swarm/scheduler/filter"
	"github.com/gorilla/mux"
	"github.com/samalba/dockerclient"
)

const APIVERSION = "1.16"

type context struct {
	cluster       *cluster.Cluster
	scheduler     *scheduler.Scheduler
	eventsHandler *eventsHandler
	debug         bool
	version       string
	tlsConfig     *tls.Config
}

type handler func(c *context, w http.ResponseWriter, r *http.Request)

// GET /info
func getInfo(c *context, w http.ResponseWriter, r *http.Request) {
	nodes := c.cluster.Nodes()
	driverStatus := [][2]string{{"\bNodes", fmt.Sprintf("%d", len(nodes))}}

	for _, node := range nodes {
		driverStatus = append(driverStatus, [2]string{node.Name, node.Addr})
	}
	info := struct {
		Containers      int
		DriverStatus    [][2]string
		NEventsListener int
		Debug           bool
	}{
		len(c.cluster.Containers()),
		driverStatus,
		c.eventsHandler.Size(),
		c.debug,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}

// GET /version
func getVersion(c *context, w http.ResponseWriter, r *http.Request) {
	version := struct {
		Version    string
		ApiVersion string
		GoVersion  string
		GitCommit  string
		Os         string
		Arch       string
	}{
		Version:    "swarm/" + c.version,
		ApiVersion: APIVERSION,
		GoVersion:  runtime.Version(),
		GitCommit:  "n/a",
		Os:         runtime.GOOS,
		Arch:       runtime.GOARCH,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(version)
}

// GET /images/json
func getImagesJSON(c *context, w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		httpError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	filters, err := dockerfilters.FromParam(r.Form.Get("filters"))
	if err != nil {
		httpError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	accepteds, _ := filters["node"]
	images := []*dockerclient.Image{}

	for _, node := range c.cluster.Nodes() {
		if len(accepteds) != 0 {
			found := false
			for _, accepted := range accepteds {
				if accepted == node.Name || accepted == node.ID {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}

		for _, image := range node.Images() {
			images = append(images, image)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(images)
}

// GET /containers/ps
// GET /containers/json
func getContainersJSON(c *context, w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		httpError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	all := r.Form.Get("all") == "1"

	out := []*dockerclient.Container{}
	for _, container := range c.cluster.Containers() {
		tmp := (*container).Container
		// Skip stopped containers unless -a was specified.
		if !strings.Contains(tmp.Status, "Up") && !all {
			continue
		}
		if !container.Node.IsHealthy() {
			tmp.Status = "Pending"
		}
		// TODO remove the Node Name in the name when we have a good solution
		tmp.Names = make([]string, len(container.Names))
		for i, name := range container.Names {
			tmp.Names[i] = "/" + container.Node.Name + name
		}
		// insert node IP
		tmp.Ports = make([]dockerclient.Port, len(container.Ports))
		for i, port := range container.Ports {
			tmp.Ports[i] = port
			if port.IP == "0.0.0.0" {
				tmp.Ports[i].IP = container.Node.IP
			}
		}
		out = append(out, &tmp)
	}

	sort.Sort(sort.Reverse(ContainerSorter(out)))

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// GET /containers/{name:.*}/json
func getContainerJSON(c *context, w http.ResponseWriter, r *http.Request) {
	container := c.cluster.Container(mux.Vars(r)["name"])
	if container != nil {
		client, scheme := newClientAndScheme(c.tlsConfig)

		resp, err := client.Get(scheme + "://" + container.Node.Addr + "/containers/" + container.Id + "/json")
		if err != nil {
			httpError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		data, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			httpError(w, err.Error(), http.StatusInternalServerError)
			return
		}

		n, err := json.Marshal(container.Node)
		if err != nil {
			httpError(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// insert Node field
		data = bytes.Replace(data, []byte("\"Name\":\"/"), []byte(fmt.Sprintf("\"Node\":%s,\"Name\":\"/", n)), -1)

		// insert node IP
		data = bytes.Replace(data, []byte("\"HostIp\":\"0.0.0.0\""), []byte(fmt.Sprintf("\"HostIp\":%q", container.Node.IP)), -1)

		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	}
}

// POST /containers/create
func postContainersCreate(c *context, w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	var (
		config dockerclient.ContainerConfig
		name   = r.Form.Get("name")
	)

	if err := json.NewDecoder(r.Body).Decode(&config); err != nil {
		httpError(w, err.Error(), http.StatusBadRequest)
		return
	}

	if container := c.cluster.Container(name); container != nil {
		httpError(w, fmt.Sprintf("Conflict, The name %s is already assigned to %s. You have to delete (or rename) that container to be able to assign %s to a container again.", name, container.Id, name), http.StatusConflict)
		return
	}

	container, err := c.scheduler.CreateContainer(&config, name)
	if err != nil {
		httpError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, "{%q:%q}", "Id", container.Id)
	return
}

// DELETE /containers/{name:.*}
func deleteContainer(c *context, w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		httpError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	name := mux.Vars(r)["name"]
	force := r.Form.Get("force") == "1"
	container := c.cluster.Container(name)
	if container == nil {
		httpError(w, fmt.Sprintf("Container %s not found", name), http.StatusNotFound)
		return
	}
	if err := c.scheduler.RemoveContainer(container, force); err != nil {
		httpError(w, err.Error(), http.StatusInternalServerError)
		return
	}

}

// GET /events
func getEvents(c *context, w http.ResponseWriter, r *http.Request) {
	c.eventsHandler.Add(r.RemoteAddr, w)

	w.Header().Set("Content-Type", "application/json")

	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	c.eventsHandler.Wait(r.RemoteAddr)
}

// GET /_ping
func ping(c *context, w http.ResponseWriter, r *http.Request) {
	w.Write([]byte{'O', 'K'})
}

// Proxy a request to the right node and do a force refresh
func proxyContainerAndForceRefresh(c *context, w http.ResponseWriter, r *http.Request) {
	container, err := getContainerFromVars(c, mux.Vars(r))
	if err != nil {
		httpError(w, err.Error(), http.StatusNotFound)
		return
	}

	if err := proxy(c.tlsConfig, container.Node.Addr, w, r); err != nil {
		httpError(w, err.Error(), http.StatusInternalServerError)
	}

	log.Debugf("[REFRESH CONTAINER] --> %s", container.Id)
	container.Node.ForceRefreshContainer(container.Container)
}

// Proxy a request to the right node
func proxyContainer(c *context, w http.ResponseWriter, r *http.Request) {
	container, err := getContainerFromVars(c, mux.Vars(r))
	if err != nil {
		httpError(w, err.Error(), http.StatusNotFound)
		return
	}

	if err := proxy(c.tlsConfig, container.Node.Addr, w, r); err != nil {
		httpError(w, err.Error(), http.StatusInternalServerError)
	}
}

// Proxy a request to a random node
func proxyRandom(c *context, w http.ResponseWriter, r *http.Request) {
	candidates := c.cluster.Nodes()

	healthFilter := &filter.HealthFilter{}
	accepted, err := healthFilter.Filter(nil, candidates)

	if err != nil {
		httpError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := proxy(c.tlsConfig, accepted[rand.Intn(len(accepted))].Addr, w, r); err != nil {
		httpError(w, err.Error(), http.StatusInternalServerError)
	}
}

// Proxy a hijack request to the right node
func proxyHijack(c *context, w http.ResponseWriter, r *http.Request) {
	container, err := getContainerFromVars(c, mux.Vars(r))
	if err != nil {
		httpError(w, err.Error(), http.StatusNotFound)
		return
	}

	if err := hijack(c.tlsConfig, container.Node.Addr, w, r); err != nil {
		httpError(w, err.Error(), http.StatusInternalServerError)
	}
}

// Default handler for methods not supported by clustering.
func notImplementedHandler(c *context, w http.ResponseWriter, r *http.Request) {
	httpError(w, "Not supported in clustering mode.", http.StatusNotImplemented)
}

func optionsHandler(c *context, w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func writeCorsHeaders(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Access-Control-Allow-Origin", "*")
	w.Header().Add("Access-Control-Allow-Headers", "Origin, X-Requested-With, Content-Type, Accept")
	w.Header().Add("Access-Control-Allow-Methods", "GET, POST, DELETE, PUT, OPTIONS")
}

func httpError(w http.ResponseWriter, err string, status int) {
	log.Error(err)
	http.Error(w, err, status)
}

func createRouter(c *context, enableCors bool) *mux.Router {
	r := mux.NewRouter()
	m := map[string]map[string]handler{
		"GET": {
			"/_ping":                          ping,
			"/events":                         getEvents,
			"/info":                           getInfo,
			"/version":                        getVersion,
			"/images/json":                    getImagesJSON,
			"/images/viz":                     notImplementedHandler,
			"/images/search":                  proxyRandom,
			"/images/get":                     notImplementedHandler,
			"/images/{name:.*}/get":           notImplementedHandler,
			"/images/{name:.*}/history":       notImplementedHandler,
			"/images/{name:.*}/json":          notImplementedHandler,
			"/containers/ps":                  getContainersJSON,
			"/containers/json":                getContainersJSON,
			"/containers/{name:.*}/export":    proxyContainer,
			"/containers/{name:.*}/changes":   proxyContainer,
			"/containers/{name:.*}/json":      getContainerJSON,
			"/containers/{name:.*}/top":       proxyContainer,
			"/containers/{name:.*}/logs":      proxyContainer,
			"/containers/{name:.*}/attach/ws": notImplementedHandler,
			"/exec/{execid:.*}/json":          proxyContainer,
		},
		"POST": {
			"/auth":                         proxyRandom,
			"/commit":                       notImplementedHandler,
			"/build":                        notImplementedHandler,
			"/images/create":                notImplementedHandler,
			"/images/load":                  notImplementedHandler,
			"/images/{name:.*}/push":        notImplementedHandler,
			"/images/{name:.*}/tag":         notImplementedHandler,
			"/containers/create":            postContainersCreate,
			"/containers/{name:.*}/kill":    proxyContainer,
			"/containers/{name:.*}/pause":   proxyContainer,
			"/containers/{name:.*}/unpause": proxyContainer,
			"/containers/{name:.*}/restart": proxyContainer,
			"/containers/{name:.*}/start":   proxyContainer,
			"/containers/{name:.*}/stop":    proxyContainer,
			"/containers/{name:.*}/wait":    proxyContainer,
			"/containers/{name:.*}/resize":  proxyContainer,
			"/containers/{name:.*}/attach":  proxyHijack,
			"/containers/{name:.*}/copy":    proxyContainer,
			"/containers/{name:.*}/exec":    proxyContainerAndForceRefresh,
			"/exec/{execid:.*}/start":       proxyHijack,
			"/exec/{execid:.*}/resize":      proxyContainer,
		},
		"DELETE": {
			"/containers/{name:.*}": deleteContainer,
			"/images/{name:.*}":     notImplementedHandler,
		},
		"OPTIONS": {
			"": optionsHandler,
		},
	}

	for method, routes := range m {
		for route, fct := range routes {
			log.Debugf("Registering %s, %s", method, route)

			// NOTE: scope issue, make sure the variables are local and won't be changed
			localRoute := route
			localFct := fct
			wrap := func(w http.ResponseWriter, r *http.Request) {
				log.Infof("%s %s", r.Method, r.RequestURI)
				if enableCors {
					writeCorsHeaders(w, r)
				}
				localFct(c, w, r)
			}
			localMethod := method

			// add the new route
			r.Path("/v{version:[0-9.]+}" + localRoute).Methods(localMethod).HandlerFunc(wrap)
			r.Path(localRoute).Methods(localMethod).HandlerFunc(wrap)
		}
	}

	return r
}
