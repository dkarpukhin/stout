package porto

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	apexlog "github.com/apex/log"
	"github.com/mitchellh/mapstructure"
	"golang.org/x/net/context"

	"github.com/noxiouz/stout/isolate"
	"github.com/noxiouz/stout/pkg/log"
	"github.com/noxiouz/stout/pkg/semaphore"

	"github.com/docker/distribution/manifest/schema1"
	"github.com/docker/distribution/manifest/schema2"
	"github.com/docker/distribution/reference"
	"github.com/docker/distribution/registry/client"
	"github.com/docker/distribution/registry/client/transport"
	engineref "github.com/docker/engine-api/types/reference"
	porto "github.com/yandex/porto/src/api/go"
	portorpc "github.com/yandex/porto/src/api/go/rpc"
)

type portoBoxConfig struct {
	// Directory where volumes per app are placed
	Layers string `json:"layers"`
	// Directory for containers
	Containers string `json:"containers"`
	// Path to a journal file
	Journal string `json:"journal"`

	SpawnConcurrency      uint              `json:"concurrency"`
	RegistryAuth          map[string]string `json:"registryauth"`
	DialRetries           int               `json:"dialretries"`
	CleanupEnabled        bool              `json:"cleanupenabled"`
	SetImgURI             bool              `json:"setimguri"`
	WeakEnabled           bool              `json:"weakenabled"`
	Gc                    bool              `json:"gc"`
	WaitLoopStepSec       uint              `json:"waitloopstepsec"`
	DefaultUlimits        string            `json:"defaultulimits"`
	VolumeBackend         string            `json:"volumebackend"`
	DefaultResolvConf     string            `json:"defaultresolv_conf"`
	CocaineAppVolumeLabel string            `json:"cocaineappvolumelabel"`
	DownloadHelperCmd     string            `json:"download_helper_cmd",omitempty`
}

func (c *portoBoxConfig) String() string {
	body, err := json.Marshal(c)
	if err != nil {
		return err.Error()
	}
	return string(body)
}

func (c *portoBoxConfig) ContainerRootDir(name, containerID string) string {
	return filepath.Join(c.Containers, name, containerID)
}

// Box operates with Porto to launch containers
type Box struct {
	Name        string
	config      *portoBoxConfig
	GlobalState isolate.GlobalState
	journal     *journal

	spawnSM      semaphore.Semaphore
	transport    *http.Transport
	muContainers sync.Mutex
	containers   map[string]*container
	blobRepo     BlobRepository
	dhEnable     bool

	rootPrefix string

	onClose context.CancelFunc

	containerPropertiesAndData []string
}

const defaultVolumeBackend = "overlay"

// NewBox creates new Box
func NewBox(ctx context.Context, cfg isolate.BoxConfig, gstate isolate.GlobalState) (isolate.Box, error) {
	log.G(ctx).Info("Porto Box Initiate")
	var config = &portoBoxConfig{
		SpawnConcurrency: 5,
		DialRetries:      10,
		WaitLoopStepSec:  10,

		CleanupEnabled: true,
		WeakEnabled:    false,
		Gc:             true,
		CocaineAppVolumeLabel: "cocaine-app",
	}
	decoderConfig := mapstructure.DecoderConfig{
		WeaklyTypedInput: true,
		Result:           config,
		TagName:          "json",
	}
	decoder, err := mapstructure.NewDecoder(&decoderConfig)
	if err != nil {
		return nil, err
	}

	if err = decoder.Decode(cfg); err != nil {
		return nil, err
	}

	if config.Layers == "" {
		return nil, fmt.Errorf("option Layers is invalid or unspecified")
	}
	if config.Containers == "" {
		return nil, fmt.Errorf("option Containers is invalid or unspecified")
	}

	if config.Journal == "" {
		return nil, fmt.Errorf("option Journal is empty or unspecified")
	}

	if config.VolumeBackend == "" {
		config.VolumeBackend = defaultVolumeBackend
	}

	log.G(ctx).WithField("dir", config.Layers).Info("create directory for Layers")
	if err = os.MkdirAll(config.Layers, 0755); err != nil {
		return nil, err
	}

	log.G(ctx).WithField("dir", config.Containers).Info("create directory for Containers")
	if err = os.MkdirAll(config.Containers, 0755); err != nil {
		return nil, err
	}

	blobRepo, err := NewBlobRepository(ctx, BlobRepositoryConfig{SpoolPath: config.Layers})
	if err != nil {
		return nil, err
	}

	tr := &http.Transport{
		Dial: func(network, addr string) (net.Conn, error) {
			for i := 0; i <= config.DialRetries; i++ {
				dialer := net.Dialer{
					DualStack: true,
					KeepAlive: 10 * time.Second,
					Timeout:   5 * time.Second,
				}
				conn, err := dialer.Dial(network, addr)
				if err == nil {
					return conn, err
				}
				sleepTime := time.Duration(rand.Int63n(500)) * time.Millisecond
				log.G(ctx).WithError(err).Errorf("dial error to %s %s. Sleep %v", network, addr, sleepTime)
				time.Sleep(sleepTime)
			}
			return nil, fmt.Errorf("no retries available")
		},
		IdleConnTimeout:       120 * time.Second,
		TLSHandshakeTimeout:   60 * time.Second,
		ExpectContinueTimeout: 5 * time.Second,
	}

	portoConn, err := portoConnect()
	if err != nil {
		return nil, err
	}
	defer portoConn.Close()

	rootPrefix, err := portoConn.GetProperty("self", "absolute_name")
	if err != nil {
		return nil, err
	}
	if rootPrefix == "/" {
		rootPrefix = ""
	}

	ctx, onClose := context.WithCancel(ctx)
	name := "porto"

	var dhEnable bool = false
	if config.DownloadHelperCmd != "" {
		dhEnable = true
	}
	log.G(ctx).Debugf("download_helper is %s: %s.", dhEnable, config.DownloadHelperCmd)

	box := &Box{
		Name:        name,
		config:      config,
		GlobalState: gstate,
		journal:     newJournal(),
		transport:   tr,
		spawnSM:     semaphore.New(config.SpawnConcurrency),
		containers:  make(map[string]*container),
		onClose:     onClose,
		rootPrefix:  rootPrefix,
		dhEnable:    dhEnable,
		blobRepo:    blobRepo,
	}

	body, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	portoConfig.Set(string(body))

	if err = box.loadJournal(ctx); err != nil {
		box.Close()
		return nil, err
	}

	layers, err := portoConn.ListLayers()
	if err != nil {
		return nil, err
	}

	box.journal.UpdateFromPorto(layers)

	journalContent.Set(box.journal.String())

	go box.waitLoop(ctx)
	go box.dumpJournalEvery(ctx, time.Minute)

	return box, nil
}

func (b *Box) dumpJournalEvery(ctx context.Context, every time.Duration) {
	for {
		select {
		case <-time.After(every):
			b.dumpJournal(ctx)
		case <-ctx.Done():
			b.dumpJournal(ctx)
			return
		}
	}
}

func (b *Box) dumpJournal(ctx context.Context) (err error) {
	defer log.G(ctx).Trace("dump journal").Stop(&err)
	tempfile, err := ioutil.TempFile(filepath.Dir(b.config.Journal), "portojournalbak")
	if err != nil {
		return err
	}
	defer os.Remove(tempfile.Name())
	defer tempfile.Close()

	if err = b.journal.Dump(tempfile); err != nil {
		return err
	}

	if err = os.Rename(tempfile.Name(), b.config.Journal); err != nil {
		return err
	}

	return nil
}

func (b *Box) loadJournal(ctx context.Context) error {
	f, err := os.Open(b.config.Journal)
	if err != nil {
		log.G(ctx).Warnf("unable to open Journal file: %v", err)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	if err = b.journal.Load(f); err != nil {
		log.G(ctx).WithError(err).Error("unable to load Journal")
		return err
	}

	return nil
}

func (b *Box) waitLoop(ctx context.Context) {
	log.G(ctx).Info("start waitLoop")
	var (
		portoConn porto.API
		err       error
	)

	closed := func(portoConn porto.API) bool {
		select {
		case <-ctx.Done():
			if portoConn != nil {
				portoConn.Close()
			}
			return true
		default:
			return false
		}
	}

	log.G(ctx).Info("waitLoop: connect to Portod before gc")
	portoConn, err = portoConnect()
	if err != nil {
		log.G(ctx).WithError(err).Warn("unable to connect to Portod")
	}

	if b.config.Gc {
		// In future we can make another loop for gc with pattern checking like:
		// rePattern, err := regexp.Compile("^.*_[0-9a-f]{6}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$")
		// Now we just try clean trash one time without error handle.
		containerNames, err := portoConn.List()
		if err != nil {
			log.G(ctx).Warnf("unable to list porto containers for gc: %v", err)
		}
		usedAllocations, stat, errUsedAllocs := b.GlobalState.Mtn.UsedAllocations(ctx)
		if errUsedAllocs != nil {
			log.G(ctx).Errorf("Cant get UsedAllocations(). Err: %s. Stat: %s.", errUsedAllocs, stat)
			return
		}
		log.G(ctx).Debugf("Allocation statistic: %s.", stat)
		var ips []string
		for _, name := range containerNames {
			containerState, _ := portoConn.GetProperty(name, "state")
			if containerState == "dead" {
				log.G(ctx).Debugf("At gc state destroy dead container: %s", name)
				portoConn.Destroy(name)
			} else if containerState == "stopped" {
				log.G(ctx).Debugf("At gc state destroy stopped container: %s", name)
				portoConn.Destroy(name)
			} else if containerState == "meta" {
				continue
			} else if containerState == "running" || containerState == "starting" {
				containerIp, _ := portoConn.GetProperty(name, "ip")
				if len(containerIp) > 2 {
					ips = append(ips, containerIp)
				}
			}
		}
		if len(usedAllocations) > 0 && len(ips) > 0 {
			for i := 0; i < len(usedAllocations); i++ {
				usedAllocation := usedAllocations[i]
				for _, ip := range ips {
					if usedAllocation.Ip == ip {
						log.G(ctx).Debugf("At gc state we found that already runned container use used mtn allocation: %s. Its fine.", usedAllocation)
						usedAllocations = append(usedAllocations[:i], usedAllocations[i+1:]...)
						i--
						break
					}
				}
			}
		}
		if len(usedAllocations) > 0 {
			log.G(ctx).Debugf("At gc state for %s some allocation still marked as \"used\": %s. So lets free them.", b.Name, usedAllocations)
			for _, usedAllocation := range usedAllocations {
				if usedAllocation.Box == b.Name {
					log.G(ctx).Debugf("Try free alloc with b.GlobalState.Mtn.UnuseAlloc(ctx, %s, %s)", usedAllocation.NetId, usedAllocation.Id)
					b.GlobalState.Mtn.UnuseAlloc(ctx, usedAllocation.NetId, usedAllocation.Id, "GC state")
				}
			}
		}
		// Now try clean unused volumes
		volumes, errLv := portoConn.ListVolumes("", "")
		if errLv != nil {
			log.G(ctx).Debugf("At gc state for ListVolumes() we get that error: %s", errLv)
		} else {
			for _, volume := range volumes {
				if volume.Properties["Private"] == b.config.CocaineAppVolumeLabel {
					portoConn.UnlinkVolume(volume.Path, "***")
				}
			}
		}
	}

LOOP:
	for {
		log.G(ctx).Debugf("next iteration of waitLoop will started after %d second of sleep.", b.config.WaitLoopStepSec)
		time.Sleep(time.Duration(b.config.WaitLoopStepSec) * time.Second)
		if closed(portoConn) {
			return
		}
		// Connect to Porto if we have not connected yet.
		// In case of error: wait either a fixed timeout or closing of Box
		if portoConn == nil {
			log.G(ctx).Info("waitLoop: connect to Portod")
			portoConn, err = portoConnect()
			if err != nil {
				log.G(ctx).WithError(err).Warn("unable to connect to Portod")
				select {
				case <-time.After(time.Second):
					continue LOOP
				case <-ctx.Done():
					return
				}
			}
		}

		ourContainers := []string{}
		b.muContainers.Lock()
		for k, _ := range b.containers {
			ourContainers = append(ourContainers, k)
		}
		b.muContainers.Unlock()
		log.G(ctx).Debugf("That containers are being tracked now: %s", ourContainers)

		for _, ourContainer := range ourContainers {
			containerState, err := portoConn.GetProperty(ourContainer, "state")
			if err != nil {
				e := err.(*porto.Error)
				switch e.ErrName {
				case "ContainerDoesNotExist":
					b.muContainers.Lock()
					container, ok := b.containers[ourContainer]
					if ok {
						delete(b.containers, ourContainer)
					}
					rest := len(b.containers)
					b.muContainers.Unlock()
					if ok {
						log.G(ctx).WithError(err).Errorf("We take ContainerDoesNotExist exception %s for exist container %s but try kill anyway.", err, ourContainer)
						if err = container.Kill(); err != nil {
							log.G(ctx).WithError(err).Debugf("catch at try kill ContainerDoesNotExist %s", ourContainer)
						}
					}
					log.G(ctx).Debugf("%d containers are being tracked now after remove ContainerDoesNotExist %s", rest, ourContainer)
				default:
					portoConn.Close()
					portoConn = nil
					continue LOOP
				}
			}
			if containerState == "dead" {
				b.muContainers.Lock()
				container, ok := b.containers[ourContainer]
				if ok {
					delete(b.containers, ourContainer)
				}
				rest := len(b.containers)
				b.muContainers.Unlock()
				log.G(ctx).Infof("%s container have status dead now.", ourContainer)
				if ok {
					if err = container.Kill(); err != nil {
						log.G(ctx).WithError(err).Errorf("Killing %s error", ourContainer)
					}
				}
				log.G(ctx).Infof("%d containers are being tracked now", rest)
			}
		}
	}
}

func (b *Box) appGenLabel(appname string) string {
	appname = strings.Replace(appname, ":", "_", -1)
	return appname
}

// func (b *Box) appLayerName(appname string) string {
// 	if b.config.WeakEnabled {
// 		return "_weak_" + b.appGenLabel(appname) + "_" + b.journal.UUID
// 	}
// 	return b.appGenLabel(appname) + "_" + b.journal.UUID
// }

func (b *Box) addRootNamespacePrefix(container string) string {
	return filepath.Join(b.rootPrefix, container)
}

// get layers from registy
func (b *Box) getLayersViaDownloadHelper(ctx context.Context, name string, profile Profile) error {
	layers := make([]string, 0)

	portoConn, err := portoConnect()
	if err != nil {
		log.G(ctx).WithError(err).WithField("name", name).Error("Porto connection error")
		return err
	}
	defer portoConn.Close()
	for _, layer := range profile.ExtendedInfo.Layers {
		portoLayerName := fmt.Sprintf("%s_%s", layer.DigestType, layer.Digest)
		wctx, cancel := context.WithTimeout(context.Background(), 1*time.Hour)
		defer cancel()
		timeout := fmt.Sprint(300 + uint(60 * (layer.Size / (100 * 1024 * 1024) )))
		cmd := exec.CommandContext(wctx, b.config.DownloadHelperCmd, "get", "-d", b.config.Layers,
			"-t", timeout, layer.TorrentId)
		stdoutStderr, err := cmd.CombinedOutput()
		if err != nil {
			log.G(ctx).WithError(err).WithField("name", name).Errorf("When download layer via download helper. output is: %s.", stdoutStderr)
			return err
		}
		blobPath := filepath.Join(b.config.Layers, layer.Digest)
		f, err := os.Open(blobPath)
		if err != nil {
			return fmt.Errorf("ERROR when open layer %s for check hashsumm.", blobPath)
		}
		defer f.Close()
		hashSum := sha256.New()
		if _, err := io.Copy(hashSum, f); err != nil {
			return fmt.Errorf("ERROR when copy layer %s for check hashsumm.", blobPath)
		}
		digest := fmt.Sprintf("%x", hashSum.Sum(nil))
		log.G(ctx).Debugf("Layer digest: %s; Layer sha256sum: %s;", layer.Digest, digest)
		if digest != layer.Digest {
			return fmt.Errorf("ERROR hashsum missmatch, hashSum.Sum(): %s, Digest: %s.", digest, layer.Digest)
		}
		entry := log.G(ctx).WithField("layer", blobPath).Trace("Try to import layer")
		err = portoConn.ImportLayer(portoLayerName, blobPath, false)
		if err != nil && !isEqualPortoError(err, portorpc.EError_LayerAlreadyExists) {
			entry.Stop(&err)
			return err
		}
		layers = append(layers, portoLayerName)
	}
	b.journal.InsertManifestLayers(name, strings.Join(layers, ";"))
	return nil
}

// get layers from registy
func (b *Box) getLayersViaRegistry(ctx context.Context, name string, profile Profile) error {
	if profile.Registry == "" {
		log.G(ctx).WithField("name", name).Error("Registry must be non empty")
		return fmt.Errorf("Registry must be non empty")
	}
	named, err := reference.ParseNamed(filepath.Join(profile.Repository, profile.Repository, name))
	if err != nil {
		log.G(ctx).WithError(err).WithField("name", name).Error("name is invalid")
		return err
	}
	var tr http.RoundTripper
	if registryAuth, ok := b.config.RegistryAuth[profile.Registry]; ok {
		tr = transport.NewTransport(b.transport, transport.NewHeaderRequestModifier(http.Header{
			"Authorization": []string{registryAuth},
		}))
	} else {
		tr = b.transport
	}

	var registry = profile.Registry
	if !strings.HasPrefix(registry, "http") {
		registry = "https://" + registry
	}
	log.G(ctx).Debugf("Image URI generated at spawn with data: %s and %s", registry, named)

	repo, err := client.NewRepository(ctx, named, registry, tr)
	if err != nil {
		return err
	}

	tagDescriptor, err := repo.Tags(ctx).Get(ctx, engineref.GetTagFromNamedRef(named))
	if err != nil {
		return err
	}

	manifests, err := repo.Manifests(ctx)
	if err != nil {
		return err
	}

	manifest, err := manifests.Get(ctx, tagDescriptor.Digest)
	if err != nil {
		return err
	}

	var order layersOrder
	switch manifest.(type) {
	case schema1.SignedManifest, *schema1.SignedManifest:
		order = layerOrderV1
	case schema2.DeserializedManifest, *schema2.DeserializedManifest:
		order = layerOrderV2
	default:
		return fmt.Errorf("unknown manifest type %T", manifest)
	}

	layers := make([]string, 0)

	portoConn, err := portoConnect()
	if err != nil {
		log.G(ctx).WithError(err).WithField("name", name).Error("Porto connection error")
		return err
	}
	defer portoConn.Close()

	for _, descriptor := range order(manifest.References()) {
		// TODO: Add support for __weak__ layers
		layerName := descriptor.Digest.String()
		// TODO: insert check of the layer existance here
		// ListLayers is too heavy IMHO
		// if the layer presents we can skip it
		blobPath, err := b.blobRepo.Get(ctx, repo, descriptor.Digest)
		if err != nil {
			return err
		}
		entry := log.G(ctx).WithField("layer", layerName).Trace("Try to import layer")
		portoLayerName := strings.Replace(layerName, ":", "_", -1)
		err = portoConn.ImportLayer(portoLayerName, blobPath, false)
		if err != nil && !isEqualPortoError(err, portorpc.EError_LayerAlreadyExists) {
			entry.Stop(&err)
			return err
		}
		layers = append(layers, portoLayerName)
	}
	b.journal.InsertManifestLayers(name, strings.Join(layers, ";"))

	return nil
}

// Spool downloades Docker images from Distribution, builds base layer for Porto container
func (b *Box) Spool(ctx context.Context, name string, opts isolate.RawProfile) (err error) {
	defer log.G(ctx).WithField("name", name).Trace("spool").Stop(&err)
	var profile = new(Profile)

	if err = opts.DecodeTo(profile); err != nil {
		log.G(ctx).WithError(err).WithField("name", name).Info("unable to convert raw profile to Porto/Docker specific profile")
		return err
	}

	var errGet error
	layersImported := false
	if len(profile.ExtendedInfo.Layers) > 0 && b.dhEnable {
		log.G(ctx).Debugf("Try get layers via download_helper cmd: %s.", b.config.DownloadHelperCmd)
		errGet = b.getLayersViaDownloadHelper(ctx, name, *profile)
		if errGet != nil {
			log.G(ctx).Warnf("Cant get layers via download helper, name: %s, error: %s.", name, errGet)
		} else {
			layersImported = true
		}
	}
	if !layersImported {
		log.G(ctx).Debugf("Try get layers via  registry. No layers in ExtendedInfo: %s or download helper not enabled: %s.",
			profile.ExtendedInfo.Layers, b.dhEnable)
		errGet = b.getLayersViaRegistry(ctx, name, *profile)
	}
	if errGet != nil {
		log.G(ctx).Errorf("Cant Spool(), name: %s, error: %s.", name, errGet)
		return errGet
	}

	// NOTE: Not so fast, but it's important for debug
	journalContent.Set(b.journal.String())

	if b.GlobalState.Mtn.Cfg.Enable && profile.Network["mtn"] == "enable" {
		err := b.GlobalState.Mtn.BindAllocs(ctx, profile.Network["netid"])
		if err != nil {
			return fmt.Errorf("Cant bind mtn alllocaton at spool with netid: %s, and error: %s", profile.Network["netid"], err)
		}
		log.G(ctx).Debugf("Successfully call b.GlobalState.Mtn.BindAllocs() at spool %s with project id %s.", name, profile.Network["netid"])
	}
	return nil
}

// Spawn spawns new Porto container
func (b *Box) Spawn(ctx context.Context, config isolate.SpawnConfig, output io.Writer) (isolate.Process, error) {
	var profile = new(Profile)
	err := config.Opts.DecodeTo(profile)
	if err != nil {
		log.G(ctx).WithError(err).Error("unable to decode profile")
		return nil, err
	}
	start := time.Now()

	spawningQueueSize.Inc(1)
	if spawningQueueSize.Count() > 10 {
		spawningQueueSize.Dec(1)
		return nil, syscall.EAGAIN
	}

	layers := b.journal.GetManifestLayers(config.Name)
	if layers == "" {
		err := fmt.Errorf("no layers in the journal for the app")
		log.G(ctx).WithFields(apexlog.Fields{"name": config.Name, "error": err}).Error("unable to start container")
		return nil, err
	}

	ID := b.appGenLabel(config.Name) + "_" + config.Args["--uuid"]
	cfg := containerConfig{
		BoxName:        b.Name,
		Root:           filepath.Join(b.config.Containers, ID),
		ID:             b.addRootNamespacePrefix(ID),
		Layer:          layers,
		State:          b.GlobalState,
		CleanupEnabled: b.config.CleanupEnabled,
		SetImgURI:      b.config.SetImgURI,
		VolumeBackend:  b.config.VolumeBackend,
		VolumeLabel:    b.config.CocaineAppVolumeLabel,
		execInfo: execInfo{
			Profile:     profile,
			name:        config.Name,
			executable:  config.Executable,
			ulimits:     b.config.DefaultUlimits,
			resolv_conf: b.config.DefaultResolvConf,
			args:        config.Args,
			env:         config.Env,
		},
	}

	portoConn, err := portoConnect()
	if err != nil {
		return nil, err
	}
	defer portoConn.Close()

	err = b.spawnSM.Acquire(ctx)
	spawningQueueSize.Dec(1)
	if err != nil {
		return nil, isolate.ErrSpawningCancelled
	}
	defer b.spawnSM.Release()

	log.G(ctx).WithFields(apexlog.Fields{"name": config.Name, "layer": cfg.Layer, "root": cfg.Root, "id": cfg.ID}).Info("Create container")

	containersCreatedCounter.Inc(1)
	pr, err := newContainer(ctx, portoConn, cfg)
	if err != nil {
		containersErroredCounter.Inc(1)
		return nil, err
	}

	b.muContainers.Lock()
	b.containers[pr.containerID] = pr
	b.muContainers.Unlock()

	if err = pr.start(portoConn, output); err != nil {
		containersErroredCounter.Inc(1)
		pr.Cleanup(portoConn)
		return nil, err
	}
	isolate.NotifyAboutStart(output)
	totalSpawnTimer.UpdateSince(start)
	return pr, nil
}

func (b *Box) Inspect(ctx context.Context, workeruuid string) ([]byte, error) {
	b.muContainers.Lock()
	for cid, pr := range b.containers {
		if pr.uuid == workeruuid {
			b.muContainers.Unlock()

			portoConn, err := portoConnect()
			if err != nil {
				return nil, err
			}
			list := getPListAndDlist(portoConn)
			result, err := portoConn.Get([]string{cid}, list)
			if err != nil {
				return nil, err
			}

			return json.Marshal(portoData(result[cid]))
		}
	}
	b.muContainers.Unlock()
	return []byte(""), nil
}

// Close releases all resources such as idle connections from http.Transport
func (b *Box) Close() error {
	b.transport.CloseIdleConnections()
	b.GlobalState.Mtn.Db.Close()
	b.onClose()
	return nil
}
