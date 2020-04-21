package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Portshift/klar/clair"
	"github.com/Portshift/klar/forwarding"
	"github.com/Portshift/kubei/pkg/config"
	k8s_utils "github.com/Portshift/kubei/pkg/utils/k8s"
	slice_utils "github.com/Portshift/kubei/pkg/utils/slice"
	uuid "github.com/satori/go.uuid"
	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"net/http"
	"sync"
	"sync/atomic"
)

type Orchestrator struct {
	imageToScanData map[string]*scanData
	progress        ScanProgress
	status          Status
	config          *config.Config
	scanConfig      *config.ScanConfig
	clientset       kubernetes.Interface
	server          *http.Server
	sync.Mutex
}

type VulnerabilitiesScanner interface {
	Start() error
	Scan(scanConfig *config.ScanConfig) error
	ScanProgress() ScanProgress
	Status() Status
	Results() *ScanResults
	Clear()
	Stop()
}

type imagePodContext struct {
	containerName   string
	podName         string
	namespace       string
	imagePullSecret string
	imageHash       string
	podUid          string
}

type scanData struct {
	imageName  string
	contexts   []*imagePodContext // All the pods that contain this image
	scanUUID   string
	result     []*clair.Vulnerability
	resultChan chan bool
	success    bool
	completed  bool
}

const (
	ignorePodScanLabelKey   = "kubeiShouldScan"
	ignorePodScanLabelValue = "false"
)

func shouldIgnorePod(pod *corev1.Pod, ignoredNamespaces []string) bool {
	if slice_utils.ContainsString(ignoredNamespaces, pod.Namespace) {
		log.Infof("Skipping pod scan, namespace is in the ignored namespaces list.  pod=%v ,namespace=%s", pod.Name, pod.Namespace)
		return true
	}
	if pod.Labels != nil && pod.Labels[ignorePodScanLabelKey] == ignorePodScanLabelValue {
		log.Infof("Skipping pod scan, pod has an ignore label. pod=%v ,namespace=%s", pod.Name, pod.Namespace)
		return true
	}

	return false
}

func (o *Orchestrator) initScan() error {
	o.status = ScanInit

	// Get all target pods
	podList, err := o.clientset.CoreV1().Pods(o.scanConfig.TargetNamespace).List(metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list pods. namespace=%s: %v", o.scanConfig.TargetNamespace, err)
	}

	imageToScanData := make(map[string]*scanData)

	// Populate the image to scanData map from all target pods
	for _, pod := range podList.Items {
		if shouldIgnorePod(&pod, o.scanConfig.IgnoredNamespaces) {
			continue
		}
		secrets := k8s_utils.GetPodImagePullSecrets(o.clientset, pod)

		// Due to scenarios where image name in the `pod.Status.ContainerStatuses` is different
		// from image name in the `pod.Spec.Containers` we will take only image id from `pod.Status.ContainerStatuses`.
		containerNameToImageId := make(map[string]string)
		for _, container := range pod.Status.ContainerStatuses {
			containerNameToImageId[container.Name] = container.ImageID
		}

		for _, container := range pod.Spec.Containers {
			// Create pod context
			podContext := &imagePodContext{
				containerName:   container.Name,
				podName:         pod.GetName(),
				podUid:          string(pod.GetUID()),
				namespace:       pod.GetNamespace(),
				imagePullSecret: k8s_utils.GetMatchingSecretName(secrets, container.Image),
				imageHash:       getImageHash(containerNameToImageId, container),
			}
			if data, ok := imageToScanData[container.Image]; !ok {
				// Image added for the first time, create scan data and append pod context
				imageToScanData[container.Image] = &scanData{
					imageName:  container.Image,
					contexts:   []*imagePodContext{podContext},
					scanUUID:   uuid.NewV4().String(),
					resultChan: make(chan bool),
				}
			} else {
				// Image already exist in map, just append the pod context
				data.contexts = append(data.contexts, podContext)
			}
		}
	}

	o.imageToScanData = imageToScanData
	o.progress = ScanProgress{
		ImagesToScan:          uint32(len(imageToScanData)),
		ImagesStartedToScan:   0,
		ImagesCompletedToScan: 0,
	}

	log.Infof("Total %d unique images to scan", o.progress.ImagesToScan)

	return nil
}

func getImageHash(containerNameToImageId map[string]string, container corev1.Container) string {
	imageID, ok := containerNameToImageId[container.Name]
	if !ok {
		log.Warnf("Image id is missing. container=%v ,image=%v", container.Name, container.Image)
		return ""
	}

	imageHash := k8s_utils.ParseImageHash(imageID)
	if imageHash == "" {
		log.Warnf("Failed to parse image hash. container=%v ,image=%v, image id=%v", container.Name, container.Image, imageID)
		return ""
	}

	return imageHash
}

func Create(config *config.Config) *Orchestrator {
	o := &Orchestrator{
		progress: ScanProgress{},
		status:   Idle,
		config:   config,
		server:   &http.Server{Addr: ":" + config.KlarResultListenPort},
		Mutex:    sync.Mutex{},
	}

	http.HandleFunc("/result/", o.resultHttpHandler)

	return o
}

func readResultBodyData(req *http.Request) (*forwarding.ImageVulnerabilities, error) {
	decoder := json.NewDecoder(req.Body)
	var bodyData *forwarding.ImageVulnerabilities
	err := decoder.Decode(&bodyData)
	if err != nil {
		return nil, fmt.Errorf("failed to decode result: %v", err)
	}

	return bodyData, nil
}

func (o *Orchestrator) handleResult(result *forwarding.ImageVulnerabilities) error {
	scanD, ok := o.imageToScanData[result.Image]
	if !ok || scanD == nil {
		return fmt.Errorf("no scan data for image '%v'", result.Image)
	}

	if result.ScanUUID != scanD.scanUUID {
		log.Warnf("Scan UUID mismatch. image=%v, received=%v, expected=%v", result.Image, result.ScanUUID, scanD.scanUUID)
		return nil
	}

	if scanD.completed {
		log.Warnf("Duplicate result for image scan. image=%v, scan uuid=%v", result.Image, result.ScanUUID)
		return nil
	}

	scanD.completed = true
	scanD.result = result.Vulnerabilities
	scanD.success = result.Success

	if scanD.success && scanD.result == nil {
		log.Infof("No vulnerabilities found on image %v.", result.Image)
	}
	if !scanD.success {
		log.Warnf("Scan of image %v has failed! See klar-scan pod logs for more info.", result.Image)
	}

	select {
	case scanD.resultChan <- true:
	default:
		log.Warnf("Failed to notify upon received result scan. image=%v, scan-uuid=%v", result.Image, result.ScanUUID)
	}

	return nil
}

func (o *Orchestrator) resultHttpHandler(w http.ResponseWriter, r *http.Request) {
	o.Lock()
	defer o.Unlock()

	result, err := readResultBodyData(r)
	if err != nil || result == nil {
		log.Errorf("Invalid result. err=%v, result=%+v", err, result)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	log.Debugf("Result was received. image=%+v, success=%+v, scanUUID=%+v",
		result.Image, result.Success, result.ScanUUID)

	err = o.handleResult(result)
	if err != nil {
		log.Errorf("Failed to handle result. err=%v, result=%+v", err, result)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	log.Debugf("Result was added successfully. image=%+v", result.Image)
	w.WriteHeader(http.StatusAccepted)
}

func (o *Orchestrator) Start() error {
	// Create K8s clientset
	clientset, err := k8s_utils.CreateClientset()
	if err != nil {
		return fmt.Errorf("failed to create clientset: %v", err)
	}

	o.clientset = clientset
	// Start result server
	log.Infof("Starting Orchestrator server")

	return o.server.ListenAndServe()
}

func (o *Orchestrator) Stop() {
	log.Infof("Stopping Orchestrator server")
	if o.server != nil {
		if err := o.server.Shutdown(context.Background()); err != nil {
			log.Errorf("Failed to shutdown server: %v", err)
		}
	}
}

func (o *Orchestrator) Scan(scanConfig *config.ScanConfig) error {
	o.Lock()
	defer o.Unlock()

	o.scanConfig = scanConfig
	log.Infof("Start scanning...")
	err := o.initScan()
	if err != nil {
		o.status = ScanInitFailure
		return fmt.Errorf("failed to initiate scan: %v", err)
	}

	o.status = Scanning
	go o.jobBatchManagement()

	return nil
}

type ScanProgress struct {
	ImagesToScan          uint32
	ImagesStartedToScan   uint32
	ImagesCompletedToScan uint32
}

func (o *Orchestrator) ScanProgress() ScanProgress {
	return ScanProgress{
		ImagesToScan:          o.progress.ImagesToScan,
		ImagesStartedToScan:   atomic.LoadUint32(&o.progress.ImagesStartedToScan),
		ImagesCompletedToScan: atomic.LoadUint32(&o.progress.ImagesCompletedToScan),
	}
}

type Status string

const (
	Idle            Status = "Idle"
	ScanInit        Status = "ScanInit"
	ScanInitFailure Status = "ScanInitFailure"
	Scanning        Status = "Scanning"
)

func (o *Orchestrator) Status() Status {
	o.Lock()
	defer o.Unlock()

	return o.status
}

type ImageScanResult struct {
	PodName         string
	PodNamespace    string
	ImageName       string
	ContainerName   string
	ImageHash       string
	PodUid          string
	Vulnerabilities []*clair.Vulnerability
	Success         bool
}

type ScanResults struct {
	ImageScanResults []*ImageScanResult
	Progress         ScanProgress
}

func (o *Orchestrator) Results() *ScanResults {
	o.Lock()
	defer o.Unlock()
	var imageScanResults []*ImageScanResult

	for _, scanD := range o.imageToScanData {
		if !scanD.completed {
			continue
		}
		for _, context := range scanD.contexts {
			imageScanResults = append(imageScanResults, &ImageScanResult{
				PodName:         context.podName,
				PodNamespace:    context.namespace,
				ImageName:       scanD.imageName,
				ContainerName:   context.containerName,
				ImageHash:       context.imageHash,
				PodUid:          context.podUid,
				Vulnerabilities: scanD.result,
				Success:         scanD.success,
			})
		}
	}

	return &ScanResults{
		ImageScanResults: imageScanResults,
		Progress:         o.ScanProgress(),
	}
}

func (o *Orchestrator) Clear() {
	o.Lock()
	defer o.Unlock()

	o.imageToScanData = nil
	o.progress = ScanProgress{}
	o.status = Idle

	return
}
