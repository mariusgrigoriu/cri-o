package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containers/image/types"
	"github.com/containers/libpod/pkg/apparmor"
	"github.com/containers/storage/pkg/idtools"
	"github.com/cri-o/cri-o/internal/lib"
	libconfig "github.com/cri-o/cri-o/internal/lib/config"
	"github.com/cri-o/cri-o/internal/lib/sandbox"
	"github.com/cri-o/cri-o/internal/oci"
	"github.com/cri-o/cri-o/internal/pkg/signals"
	"github.com/cri-o/cri-o/internal/pkg/storage"
	"github.com/cri-o/cri-o/server/metrics"
	"github.com/cri-o/cri-o/server/useragent"
	"github.com/cri-o/ocicni/pkg/ocicni"
	"github.com/fsnotify/fsnotify"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	seccomp "github.com/seccomp/containers-golang"
	"github.com/sirupsen/logrus"
	knet "k8s.io/apimachinery/pkg/util/net"
	pb "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
	"k8s.io/kubernetes/pkg/kubelet/dockershim/network/hostport"
	"k8s.io/kubernetes/pkg/kubelet/server/streaming"
	iptablesproxy "k8s.io/kubernetes/pkg/proxy/iptables"
	utildbus "k8s.io/kubernetes/pkg/util/dbus"
	utiliptables "k8s.io/kubernetes/pkg/util/iptables"
	utilexec "k8s.io/utils/exec"
)

const (
	shutdownFile        = "/var/lib/crio/crio.shutdown"
	certRefreshInterval = time.Minute * 5
)

// StreamService implements streaming.Runtime.
type StreamService struct {
	runtimeServer       *Server // needed by Exec() endpoint
	streamServer        streaming.Server
	streamServerCloseCh chan struct{}
	streaming.Runtime
}

// Server implements the RuntimeService and ImageService
type Server struct {
	config          libconfig.Config
	seccompProfile  *seccomp.Seccomp
	stream          StreamService
	netPlugin       ocicni.CNIPlugin
	hostportManager hostport.HostPortManager

	appArmorProfile string
	hostIP          string
	bindAddress     string

	*lib.ContainerServer
	monitorsChan      chan struct{}
	defaultIDMappings *idtools.IDMappings
	systemContext     *types.SystemContext // Never nil

	updateLock sync.RWMutex

	seccompEnabled  bool
	appArmorEnabled bool
}

type certConfigCache struct {
	config  *tls.Config
	expires time.Time

	tlsCert string
	tlsKey  string
	tlsCA   string
}

// GetConfigForClient gets the tlsConfig for the streaming server.
// This allows the certs to be swapped, without shutting down crio.
func (cc *certConfigCache) GetConfigForClient(hello *tls.ClientHelloInfo) (*tls.Config, error) {
	if cc.config != nil && time.Now().Before(cc.expires) {
		return cc.config, nil
	}
	config := new(tls.Config)
	cert, err := tls.LoadX509KeyPair(cc.tlsCert, cc.tlsKey)
	if err != nil {
		return nil, err
	}
	config.Certificates = []tls.Certificate{cert}
	if len(cc.tlsCA) > 0 {
		caBytes, err := ioutil.ReadFile(cc.tlsCA)
		if err != nil {
			return nil, err
		}
		certPool := x509.NewCertPool()
		certPool.AppendCertsFromPEM(caBytes)
		config.ClientCAs = certPool
		config.ClientAuth = tls.RequireAndVerifyClientCert
	}
	cc.config = config
	cc.expires = time.Now().Add(certRefreshInterval)
	return config, nil
}

// StopStreamServer stops the stream server
func (s *Server) StopStreamServer() error {
	return s.stream.streamServer.Stop()
}

// StreamingServerCloseChan returns the close channel for the streaming server
func (s *Server) StreamingServerCloseChan() chan struct{} {
	return s.stream.streamServerCloseCh
}

// getExec returns exec stream request
func (s *Server) getExec(req *pb.ExecRequest) (*pb.ExecResponse, error) {
	return s.stream.streamServer.GetExec(req)
}

// getAttach returns attach stream request
func (s *Server) getAttach(req *pb.AttachRequest) (*pb.AttachResponse, error) {
	return s.stream.streamServer.GetAttach(req)
}

// getPortForward returns port forward stream request
func (s *Server) getPortForward(req *pb.PortForwardRequest) (*pb.PortForwardResponse, error) {
	return s.stream.streamServer.GetPortForward(req)
}

func (s *Server) restore() {
	containers, err := s.Store().Containers()
	if err != nil && !os.IsNotExist(errors.Cause(err)) {
		logrus.Warnf("could not read containers and sandboxes: %v", err)
	}
	pods := map[string]*storage.RuntimeContainerMetadata{}
	podContainers := map[string]*storage.RuntimeContainerMetadata{}
	names := map[string][]string{}
	deletedPods := map[string]bool{}
	for i := range containers {
		metadata, err2 := s.StorageRuntimeServer().GetContainerMetadata(containers[i].ID)
		if err2 != nil {
			logrus.Warnf("error parsing metadata for %s: %v, ignoring", containers[i].ID, err2)
			continue
		}
		names[containers[i].ID] = containers[i].Names
		if metadata.Pod {
			pods[containers[i].ID] = &metadata
		} else {
			podContainers[containers[i].ID] = &metadata
		}
	}

	// Go through all the pods and check if it can be restored. If an error occurs, delete the pod and any containers
	// associated with it. Release the pod and container names as well.
	for sbID, metadata := range pods {
		if err = s.LoadSandbox(sbID); err == nil {
			continue
		}
		logrus.Warnf("could not restore sandbox %s container %s: %v", metadata.PodID, sbID, err)
		for _, n := range names[sbID] {
			if err := s.Store().DeleteContainer(n); err != nil {
				logrus.Warnf("unable to delete container %s: %v", n, err)
			}
			// Release the infra container name and the pod name for future use
			if strings.Contains(n, infraName) {
				s.ReleaseContainerName(n)
			} else {
				s.ReleasePodName(n)
			}

		}
		// Go through the containers and delete any container that was under the deleted pod
		logrus.Warnf("deleting all containers under sandbox %s since it could not be restored", sbID)
		for k, v := range podContainers {
			if v.PodID == sbID {
				for _, n := range names[k] {
					if err := s.Store().DeleteContainer(n); err != nil {
						logrus.Warnf("unable to delete container %s: %v", n, err)
					}
					// Release the container name for future use
					s.ReleaseContainerName(n)
				}
			}
		}
		// Add the pod id to the list of deletedPods so we don't try to restore IPs for it later on
		deletedPods[sbID] = true
	}

	// Go through all the containers and check if it can be restored. If an error occurs, delete the conainer and
	// release the name associated with you.
	for containerID := range podContainers {
		if err := s.LoadContainer(containerID); err != nil {
			logrus.Warnf("could not restore container %s: %v", containerID, err)
			for _, n := range names[containerID] {
				if err := s.Store().DeleteContainer(n); err != nil {
					logrus.Warnf("unable to delete container %s: %v", n, err)
				}
				// Release the container name
				s.ReleaseContainerName(n)
			}
		}
	}

	// Restore sandbox IPs
	for _, sb := range s.ListSandboxes() {
		// Move on if pod was deleted
		if ok := deletedPods[sb.ID()]; ok {
			continue
		}
		ip, err := s.getSandboxIP(sb)
		if err != nil {
			logrus.Warnf("could not restore sandbox IP for %v: %v", sb.ID(), err)
		}
		sb.AddIP(ip)
	}
}

// cleanupSandboxesOnShutdown Remove all running Sandboxes on system shutdown
func (s *Server) cleanupSandboxesOnShutdown(ctx context.Context) {
	_, err := os.Stat(shutdownFile)
	if err == nil || !os.IsNotExist(err) {
		logrus.Debugf("shutting down all sandboxes, on shutdown")
		s.stopAllPodSandboxes(ctx)
		err = os.Remove(shutdownFile)
		if err != nil {
			logrus.Warnf("Failed to remove %q", shutdownFile)
		}

	}
}

// Shutdown attempts to shut down the server's storage cleanly
func (s *Server) Shutdown(ctx context.Context) error {
	// why do this on clean shutdown! we want containers left running when crio
	// is down for whatever reason no?!
	// notice this won't trigger just on system halt but also on normal
	// crio.service restart!!!
	s.cleanupSandboxesOnShutdown(ctx)
	return s.ContainerServer.Shutdown()
}

// configureMaxThreads sets the Go runtime max threads threshold
// which is 90% of the kernel setting from /proc/sys/kernel/threads-max
func configureMaxThreads() error {
	mt, err := ioutil.ReadFile("/proc/sys/kernel/threads-max")
	if err != nil {
		return err
	}
	mtint, err := strconv.Atoi(strings.TrimSpace(string(mt)))
	if err != nil {
		return err
	}
	maxThreads := (mtint / 100) * 90
	debug.SetMaxThreads(maxThreads)
	logrus.Debugf("Golang's threads limit set to %d", maxThreads)
	return nil
}

func getIDMappings(config *libconfig.Config) (*idtools.IDMappings, error) {
	if config.UIDMappings == "" || config.GIDMappings == "" {
		return nil, nil
	}

	parsedUIDsMappings, err := idtools.ParseIDMap(strings.Split(config.UIDMappings, ","), "UID")
	if err != nil {
		return nil, err
	}
	parsedGIDsMappings, err := idtools.ParseIDMap(strings.Split(config.GIDMappings, ","), "GID")
	if err != nil {
		return nil, err
	}

	return idtools.NewIDMappingsFromMaps(parsedUIDsMappings, parsedGIDsMappings), nil
}

// New creates a new Server with the provided context, systemContext,
// configPath and configuration
func New(
	ctx context.Context,
	systemContext *types.SystemContext,
	configPath string,
	configIface libconfig.Iface,
) (*Server, error) {

	if configIface == nil || configIface.GetData() == nil {
		return nil, fmt.Errorf("provided configuration interface or its data is nil")
	}
	config := configIface.GetData()

	// Make a copy of systemContext we can safely modify, and which is never nil. (Note that this is a shallow copy!)
	sc := types.SystemContext{}
	if systemContext != nil {
		sc = *systemContext
	}
	systemContext = &sc

	systemContext.AuthFilePath = config.GlobalAuthFile
	systemContext.DockerRegistryUserAgent = useragent.Get(ctx)
	systemContext.SignaturePolicyPath = config.SignaturePolicyPath

	if err := os.MkdirAll(config.ContainerAttachSocketDir, 0755); err != nil {
		return nil, err
	}

	// This is used to monitor container exits using inotify
	if err := os.MkdirAll(config.ContainerExitsDir, 0755); err != nil {
		return nil, err
	}
	containerServer, err := lib.New(ctx, systemContext, configIface)
	if err != nil {
		return nil, err
	}

	netPlugin, err := ocicni.InitCNI("", config.NetworkDir, config.PluginDirs...)
	if err != nil {
		return nil, err
	}
	iptInterface := utiliptables.New(utilexec.New(), utildbus.New(), utiliptables.ProtocolIpv4)
	if _, err := iptInterface.EnsureChain(utiliptables.TableNAT, iptablesproxy.KubeMarkMasqChain); err != nil {
		logrus.Warnf("unable to ensure iptables chain: %v", err)
	}
	hostportManager := hostport.NewHostportManager(iptInterface)

	idMappings, err := getIDMappings(config)
	if err != nil {
		return nil, err
	}

	s := &Server{
		ContainerServer:   containerServer,
		netPlugin:         netPlugin,
		hostportManager:   hostportManager,
		config:            *config,
		seccompEnabled:    seccomp.IsEnabled(),
		appArmorEnabled:   apparmor.IsEnabled(),
		appArmorProfile:   config.ApparmorProfile,
		monitorsChan:      make(chan struct{}),
		defaultIDMappings: idMappings,
		systemContext:     systemContext,
	}

	if s.seccompEnabled {
		if config.SeccompProfile != "" {
			seccompProfile, fileErr := ioutil.ReadFile(config.SeccompProfile)
			if fileErr != nil {
				return nil, fmt.Errorf("opening seccomp profile (%s) failed: %v",
					config.SeccompProfile, fileErr)
			}
			var seccompConfig seccomp.Seccomp
			if jsonErr := json.Unmarshal(seccompProfile, &seccompConfig); jsonErr != nil {
				return nil, fmt.Errorf("decoding seccomp profile failed: %v", jsonErr)
			}
			logrus.Infof("using seccomp profile %q", config.SeccompProfile)
			s.seccompProfile = &seccompConfig
		} else {
			logrus.Infof("no seccomp profile specified, using the internal default")
			s.seccompProfile = seccomp.DefaultProfile()
		}
	}

	if s.appArmorEnabled && config.ApparmorProfile == libconfig.DefaultApparmorProfile {
		logrus.Infof("installing default apparmor profile: %v", libconfig.DefaultApparmorProfile)
		if err := apparmor.InstallDefault(libconfig.DefaultApparmorProfile); err != nil {
			return nil, fmt.Errorf("ensuring the default apparmor profile %q is installed failed: %v", libconfig.DefaultApparmorProfile, err)
		}
	} else {
		logrus.Infof("assuming user-provided apparmor profile: %v", config.ApparmorProfile)
	}

	if err := configureMaxThreads(); err != nil {
		return nil, err
	}

	s.restore()
	s.cleanupSandboxesOnShutdown(ctx)

	hostIP := net.ParseIP(config.HostIP)
	if hostIP == nil {
		hostIP, err = knet.ChooseBindAddress(nil)
		if err != nil {
			return nil, err
		}
	}
	bindAddress := net.ParseIP(config.StreamAddress)
	if bindAddress == nil {
		bindAddress = hostIP
	}
	s.bindAddress = bindAddress.String()
	s.hostIP = hostIP.String()

	_, err = net.LookupPort("tcp", config.StreamPort)
	if err != nil {
		return nil, err
	}

	// Prepare streaming server
	streamServerConfig := streaming.DefaultConfig
	streamServerConfig.Addr = net.JoinHostPort(bindAddress.String(), config.StreamPort)
	if config.StreamEnableTLS {
		certCache := &certConfigCache{
			tlsCert: config.StreamTLSCert,
			tlsKey:  config.StreamTLSKey,
			tlsCA:   config.StreamTLSCA,
		}
		// We add the certs to the config, even thought the config is dynamic, because
		// the http package method, ServeTLS, checks to make sure there is a cert in the
		// config or it throws an error.
		cert, err := tls.LoadX509KeyPair(config.StreamTLSCert, config.StreamTLSKey)
		if err != nil {
			return nil, err
		}
		streamServerConfig.TLSConfig = &tls.Config{
			GetConfigForClient: certCache.GetConfigForClient,
			Certificates:       []tls.Certificate{cert},
		}
	}
	s.stream.runtimeServer = s
	s.stream.streamServer, err = streaming.NewServer(streamServerConfig, s.stream)
	if err != nil {
		return nil, fmt.Errorf("unable to create streaming server")
	}

	s.stream.streamServerCloseCh = make(chan struct{})
	go func() {
		defer close(s.stream.streamServerCloseCh)
		if err := s.stream.streamServer.Start(true); err != nil && err != http.ErrServerClosed {
			logrus.Errorf("Failed to start streaming server: %v", err)
		}
	}()

	logrus.Debugf("sandboxes: %v", s.ContainerServer.ListSandboxes())

	// Start a configuration watcher for the default config
	if _, err := s.StartConfigWatcher(configPath, s.config.Reload); err != nil {
		logrus.Warnf("unable to start config watcher for file %q: %v",
			configPath, err)
	}

	return s, nil
}

func (s *Server) addSandbox(sb *sandbox.Sandbox) error {
	return s.ContainerServer.AddSandbox(sb)
}

func (s *Server) getSandbox(id string) *sandbox.Sandbox {
	return s.ContainerServer.GetSandbox(id)
}

func (s *Server) removeSandbox(id string) error {
	return s.ContainerServer.RemoveSandbox(id)
}

func (s *Server) addContainer(c *oci.Container) {
	s.ContainerServer.AddContainer(c)
}

func (s *Server) addInfraContainer(c *oci.Container) {
	s.ContainerServer.AddInfraContainer(c)
}

func (s *Server) getInfraContainer(id string) *oci.Container {
	return s.ContainerServer.GetInfraContainer(id)
}

func (s *Server) removeContainer(c *oci.Container) {
	s.ContainerServer.RemoveContainer(c)
}

func (s *Server) removeInfraContainer(c *oci.Container) {
	s.ContainerServer.RemoveInfraContainer(c)
}

func (s *Server) getPodSandboxFromRequest(podSandboxID string) (*sandbox.Sandbox, error) {
	if podSandboxID == "" {
		return nil, sandbox.ErrIDEmpty
	}

	sandboxID, err := s.PodIDIndex().Get(podSandboxID)
	if err != nil {
		return nil, fmt.Errorf("PodSandbox with ID starting with %s not found: %v", podSandboxID, err)
	}

	sb := s.getSandbox(sandboxID)
	if sb == nil {
		return nil, fmt.Errorf("specified pod sandbox not found: %s", sandboxID)
	}
	return sb, nil
}

// CreateMetricsEndpoint creates a /metrics endpoint
// for prometheus monitoring
func (s *Server) CreateMetricsEndpoint() (*http.ServeMux, error) {
	metrics.Register()
	mux := &http.ServeMux{}
	mux.Handle("/metrics", promhttp.Handler())
	return mux, nil
}

// StopMonitors stops all the monitors
func (s *Server) StopMonitors() {
	close(s.monitorsChan)
}

// MonitorsCloseChan returns the close chan for the exit monitor
func (s *Server) MonitorsCloseChan() chan struct{} {
	return s.monitorsChan
}

// StartExitMonitor start a routine that monitors container exits
// and updates the container status
func (s *Server) StartExitMonitor() {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		logrus.Fatalf("Failed to create new watch: %v", err)
	}
	defer watcher.Close()

	done := make(chan struct{})
	go func() {
		for {
			select {
			case event := <-watcher.Events:
				logrus.Debugf("event: %v", event)
				if event.Op&fsnotify.Create == fsnotify.Create {
					containerID := filepath.Base(event.Name)
					logrus.Debugf("container or sandbox exited: %v", containerID)
					c := s.GetContainer(containerID)
					if c != nil {
						logrus.Debugf("container exited and found: %v", containerID)
						err := s.Runtime().UpdateContainerStatus(c)
						if err != nil {
							logrus.Warnf("Failed to update container status %s: %v", containerID, err)
						} else if err := s.ContainerStateToDisk(c); err != nil {
							logrus.Warnf("unable to write containers %s state to disk: %v", c.ID(), err)
						}
					} else {
						sb := s.GetSandbox(containerID)
						if sb != nil {
							c := sb.InfraContainer()
							if c == nil {
								logrus.Warnf("no infra container set for sandbox: %v", containerID)
								continue
							}
							logrus.Debugf("sandbox exited and found: %v", containerID)
							err := s.Runtime().UpdateContainerStatus(c)
							if err != nil {
								logrus.Warnf("Failed to update sandbox infra container status %s: %v", c.ID(), err)
							} else if err := s.ContainerStateToDisk(c); err != nil {
								logrus.Warnf("unable to write containers %s state to disk: %v", c.ID(), err)
							}
						}
					}
				}
			case err := <-watcher.Errors:
				logrus.Debugf("watch error: %v", err)
				return
			case <-s.monitorsChan:
				logrus.Debug("closing exit monitor...")
				close(done)
				return
			}
		}
	}()
	if err := watcher.Add(s.config.ContainerExitsDir); err != nil {
		logrus.Errorf("watcher.Add(%q) failed: %s", s.config.ContainerExitsDir, err)
		close(done)
	}
	<-done
}

// StartConfigWatcher starts a new watching go routine for the provided
// `fileName` and `reloadFunc`. The method errors if the given fileName does
// not exist or is not accessible.
func (s *Server) StartConfigWatcher(
	fileName string,
	reloadFunc func(string) error,
) (chan os.Signal, error) {
	// Validate the arguments
	if _, err := os.Stat(fileName); err != nil {
		return nil, err
	}
	if reloadFunc == nil {
		return nil, fmt.Errorf("provided reload closure is nil")
	}

	// Setup the signal notifier
	c := make(chan os.Signal, 1)
	signal.Notify(c, signals.Hup)

	go func() {
		for {
			// Block until the signal is received
			<-c
			logrus.Infof("reloading configuration %q", fileName)
			if err := reloadFunc(fileName); err != nil {
				logrus.Errorf("unable to reload configuration: %v", err)
				continue
			}
		}
	}()

	logrus.Debugf("registered SIGHUP watcher for file %q", fileName)
	return c, nil
}
