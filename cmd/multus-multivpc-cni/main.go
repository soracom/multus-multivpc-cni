package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/containernetworking/cni/pkg/invoke"
	"github.com/containernetworking/cni/pkg/skel"
	cnitypes "github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/vishvananda/netlink"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/soracom/multus-multivpc-cni/pkg/netns"
	syncpb "github.com/soracom/multus-multivpc-cni/proto/syncpb"
)

const (
	defaultSocketPath        = "/run/multivpc-eni-agent.sock"
	defaultInterfaceName     = "net1"
	defaultCacheDir          = "/var/lib/cni/multivpc"
	defaultNetworkProfileAnn = "cni.soracom.io/multus-multivpc-profile"
	defaultWaitTimeout       = 60 * time.Second
	defaultPollInterval      = 200 * time.Millisecond

	pluginName         = "multus-multivpc-cni"
	ipvlanDelegateName = "multivpc-ipvlan"
	sbrDelegateName    = "multivpc-sbr"
)

type pluginConf struct {
	cnitypes.NetConf
	SocketPath               string           `json:"socketPath"`
	InterfaceName            string           `json:"interfaceName"`
	CacheDir                 string           `json:"cacheDir"`
	NetworkProfile           string           `json:"networkProfile"`
	NetworkProfileAnnotation string           `json:"networkProfileAnnotation"`
	NetworkProfileArg        string           `json:"networkProfileArg"`
	NetworkProfileDefault    string           `json:"networkProfileDefault"`
	Delegate                 map[string]any   `json:"delegate"`
	PostDelegates            []map[string]any `json:"postDelegates"`
	WaitTimeoutSeconds       int              `json:"waitTimeoutSeconds"`
	PollIntervalMilliseconds int              `json:"pollIntervalMilliseconds"`
	RuntimeConfig            map[string]any   `json:"runtimeConfig,omitempty"`
	runtimeConfig            map[string]any
}

type kubernetesArgs struct {
	cnitypes.CommonArgs
	K8S_POD_NAME               cnitypes.UnmarshallableString
	K8S_POD_NAMESPACE          cnitypes.UnmarshallableString
	K8S_POD_UID                cnitypes.UnmarshallableString
	K8S_NODE_NAME              cnitypes.UnmarshallableString
	K8S_POD_INFRA_CONTAINER_ID cnitypes.UnmarshallableString
}

type cacheEntry struct {
	PodUID         string `json:"podUID"`
	PodName        string `json:"podName"`
	Namespace      string `json:"namespace"`
	NetworkProfile string `json:"networkProfile"`
	InterfaceName  string `json:"interfaceName"`
	ContainerID    string `json:"containerID"`
}

func main() {
	flag.Parse()

	skel.PluginMainFuncs(skel.CNIFuncs{
		Add:   cmdAdd,
		Check: cmdCheck,
		Del:   cmdDel,
	}, version.All, "Multus delegate for Multi-VPC ENI attachments with embedded ipvlan chaining")
}

func cmdAdd(args *skel.CmdArgs) error {
	conf, err := loadConfig(args.StdinData)
	if err != nil {
		return err
	}

	ifName := args.IfName
	if ifName == "" {
		if ifName = conf.InterfaceName; ifName == "" {
			ifName = defaultInterfaceName
		}
	}

	k8sArgs, err := loadKubernetesArgs(args.Args)
	if err != nil {
		return err
	}

	networkProfile, err := resolveNetworkProfile(conf, args.Args)
	if err != nil {
		return err
	}
	if networkProfile == "" {
		return errors.New("network profile could not be determined")
	}

	ctx, cancel := context.WithTimeout(context.Background(), conf.waitTimeout())
	defer cancel()

	conn, client, err := dialAgent(ctx, conf.SocketPath)
	if err != nil {
		return err
	}
	defer func() {
		if err := conn.Close(); err != nil {
			log.Printf("close agent connection: %v", err)
		}
	}()

	prepReq := &syncpb.ReportRequest{
		NodeName:       string(k8sArgs.K8S_NODE_NAME),
		Namespace:      string(k8sArgs.K8S_POD_NAMESPACE),
		PodName:        string(k8sArgs.K8S_POD_NAME),
		PodUid:         string(k8sArgs.K8S_POD_UID),
		ContainerId:    args.ContainerID,
		InterfaceName:  ifName,
		NetworkProfile: networkProfile,
	}

	if err := callReport(ctx, client, prepReq); err != nil {
		return err
	}

	state, err := waitForAttachedState(ctx, client, prepReq.PodUid, ifName, networkProfile, conf.pollInterval())
	if err != nil {
		return err
	}

	masterName, err := waitForHostInterface(ctx, state.GetMacAddress(), conf.pollInterval())
	if err != nil {
		return err
	}

	result, err := runDelegateChain(ctx, conf, masterName)
	if err != nil {
		return err
	}

	iface, ipv4, ipv6, err := collectInterfaceDetails(args.Netns, ifName, result)
	if err != nil {
		return err
	}

	reportReq := &syncpb.ReportRequest{
		NodeName:       string(k8sArgs.K8S_NODE_NAME),
		Namespace:      string(k8sArgs.K8S_POD_NAMESPACE),
		PodName:        string(k8sArgs.K8S_POD_NAME),
		PodUid:         string(k8sArgs.K8S_POD_UID),
		ContainerId:    args.ContainerID,
		InterfaceName:  ifName,
		NetworkProfile: networkProfile,
		Ipv4Addresses:  ipsToStrings(ipv4),
		Ipv6Addresses:  ipsToStrings(ipv6),
		MacAddress:     iface.Hardware.String(),
	}

	if err := callReport(ctx, client, reportReq); err != nil {
		return err
	}

	entry := cacheEntry{
		PodUID:         reportReq.PodUid,
		PodName:        reportReq.PodName,
		Namespace:      reportReq.Namespace,
		NetworkProfile: reportReq.NetworkProfile,
		InterfaceName:  reportReq.InterfaceName,
		ContainerID:    args.ContainerID,
	}
	if err := writeCacheEntry(conf.CacheDir, entry); err != nil {
		return err
	}

	return cnitypes.PrintResult(result, conf.CNIVersion)
}

func cmdDel(args *skel.CmdArgs) error {
	conf, err := loadConfig(args.StdinData)
	if err != nil {
		return err
	}

	entry, path, err := readCacheEntry(conf.CacheDir, args.ContainerID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), conf.waitTimeout())
	defer cancel()

	// Best-effort lookup for master device to assist delegates on DEL.
	var masterName string
	conn, client, err := dialAgent(ctx, conf.SocketPath)
	if err == nil {
		defer func() {
			if err := conn.Close(); err != nil {
				log.Printf("close agent connection: %v", err)
			}
		}()
		lookupCtx, cancelLookup := context.WithTimeout(ctx, 5*time.Second)
		defer cancelLookup()

		state, err := waitForAttachedState(lookupCtx, client, entry.PodUID, entry.InterfaceName, entry.NetworkProfile, conf.pollInterval())
		if err == nil && state.GetMacAddress() != "" {
			if name, err := waitForHostInterface(ctx, state.GetMacAddress(), conf.pollInterval()); err == nil {
				masterName = name
			}
		}
	}

	if err := runDelegateDelChain(ctx, conf, masterName); err != nil {
		log.Printf("delegate DEL failed: %v", err)
	}

	if client != nil {
		if err := callRelease(ctx, client, &syncpb.ReleaseRequest{
			Namespace:      entry.Namespace,
			PodName:        entry.PodName,
			PodUid:         entry.PodUID,
			InterfaceName:  entry.InterfaceName,
			NetworkProfile: entry.NetworkProfile,
		}); err != nil {
			log.Printf("release interface failed: %v", err)
		}
	}

	return os.Remove(path)
}

func cmdCheck(args *skel.CmdArgs) error {
	_, err := loadConfig(args.StdinData)
	return err
}

func loadConfig(stdin []byte) (*pluginConf, error) {
	conf := &pluginConf{}
	if err := json.Unmarshal(stdin, conf); err != nil {
		return nil, fmt.Errorf("unmarshal plugin conf: %w", err)
	}
	if conf.RawPrevResult != nil {
		_ = version.ParsePrevResult(&conf.NetConf)
	}
	var raw map[string]any
	if err := json.Unmarshal(stdin, &raw); err == nil {
		if rc, ok := raw["runtimeConfig"].(map[string]any); ok {
			conf.runtimeConfig = rc
		}
	}
	if conf.SocketPath == "" {
		conf.SocketPath = defaultSocketPath
	}
	if conf.InterfaceName == "" {
		conf.InterfaceName = defaultInterfaceName
	}
	if conf.CacheDir == "" {
		conf.CacheDir = defaultCacheDir
	}
	if conf.NetworkProfileAnnotation == "" {
		conf.NetworkProfileAnnotation = defaultNetworkProfileAnn
	}
	if conf.CNIVersion == "" {
		conf.CNIVersion = version.Current()
	}
	if conf.WaitTimeoutSeconds <= 0 {
		conf.WaitTimeoutSeconds = int(defaultWaitTimeout.Seconds())
	}
	if conf.PollIntervalMilliseconds <= 0 {
		conf.PollIntervalMilliseconds = int(defaultPollInterval / time.Millisecond)
	}
	// Always force a deterministic name to avoid missing network name errors.
	conf.Name = pluginName
	return conf, nil
}

func loadKubernetesArgs(argStr string) (*kubernetesArgs, error) {
	args := &kubernetesArgs{}
	if err := cnitypes.LoadArgs(argStr, args); err != nil {
		return nil, fmt.Errorf("parse CNI args: %w", err)
	}
	return args, nil
}

func resolveNetworkProfile(conf *pluginConf, argStr string) (string, error) {
	if conf.NetworkProfile != "" {
		return conf.NetworkProfile, nil
	}
	if ann := profileFromRuntime(conf, conf.NetworkProfileAnnotation); ann != "" {
		return ann, nil
	}
	if conf.NetworkProfileArg != "" {
		if val := parseArgValue(argStr, conf.NetworkProfileArg); val != "" {
			return val, nil
		}
	}
	return conf.NetworkProfileDefault, nil
}

func profileFromRuntime(conf *pluginConf, annotation string) string {
	if conf.runtimeConfig == nil {
		return ""
	}
	k8sSection, ok := conf.runtimeConfig["k8s"].(map[string]any)
	if !ok {
		return ""
	}
	annotations, ok := k8sSection["podAnnotations"].(map[string]any)
	if !ok {
		return ""
	}
	if val, ok := annotations[annotation]; ok {
		if str, ok := val.(string); ok {
			return str
		}
	}
	return ""
}

func parseArgValue(argStr, key string) string {
	for _, segment := range strings.Split(argStr, ";") {
		kv := strings.SplitN(segment, "=", 2)
		if len(kv) != 2 {
			continue
		}
		if kv[0] == key {
			return kv[1]
		}
	}
	return ""
}

func dialAgent(ctx context.Context, socketPath string) (*grpc.ClientConn, syncpb.IpSyncServiceClient, error) {
	conn, err := grpc.NewClient(
		"unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(dialCtx context.Context, addr string) (net.Conn, error) {
			d := net.Dialer{}
			return d.DialContext(dialCtx, "unix", strings.TrimPrefix(addr, "unix://"))
		}),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("dial agent: %w", err)
	}
	return conn, syncpb.NewIpSyncServiceClient(conn), nil
}

func callReport(ctx context.Context, client syncpb.IpSyncServiceClient, req *syncpb.ReportRequest) error {
	resp, err := client.ReportInterface(ctx, req)
	if err != nil {
		return fmt.Errorf("report interface: %w", err)
	}
	if !resp.GetAccepted() {
		return fmt.Errorf("agent rejected request: %s", resp.GetMessage())
	}
	return nil
}

func callRelease(ctx context.Context, client syncpb.IpSyncServiceClient, req *syncpb.ReleaseRequest) error {
	resp, err := client.ReleaseInterface(ctx, req)
	if err != nil {
		return fmt.Errorf("release interface: %w", err)
	}
	if !resp.GetAccepted() {
		return fmt.Errorf("agent rejected release: %s", resp.GetMessage())
	}
	return nil
}

func waitForAttachedState(ctx context.Context, client syncpb.IpSyncServiceClient, podUID, ifName, profile string, interval time.Duration) (*syncpb.InterfaceState, error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		req := &syncpb.GetStatusRequest{
			PodUid:         podUID,
			InterfaceName:  ifName,
			NetworkProfile: profile,
		}
		resp, err := client.GetStatus(ctx, req)
		if err == nil {
			for _, st := range resp.Interfaces {
				if st.GetStatus() == "attached" && st.GetMacAddress() != "" {
					return st, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			if err != nil {
				return nil, fmt.Errorf("wait for attached state: %w", err)
			}
			return nil, fmt.Errorf("wait for attached state: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func waitForHostInterface(ctx context.Context, mac string, interval time.Duration) (string, error) {
	if mac == "" {
		return "", errors.New("mac address not provided")
	}
	target, err := net.ParseMAC(mac)
	if err != nil {
		return "", fmt.Errorf("parse mac: %w", err)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		links, err := netlink.LinkList()
		if err == nil {
			for _, link := range links {
				if link.Attrs() != nil && bytes.Equal(link.Attrs().HardwareAddr, target) {
					if err := netlink.LinkSetUp(link); err != nil {
						log.Printf("failed to set link %s up: %v", link.Attrs().Name, err)
					}
					return link.Attrs().Name, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			if err != nil {
				return "", fmt.Errorf("wait for host interface: %w", err)
			}
			return "", fmt.Errorf("wait for host interface: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func runDelegateChain(ctx context.Context, conf *pluginConf, masterName string) (cnitypes.Result, error) {
	baseDelegate := deepCopyMap(conf.Delegate)
	if baseDelegate == nil {
		baseDelegate = map[string]any{}
	}
	baseDelegate = configureDelegate(conf, masterName, baseDelegate, ipvlanDelegateName)

	result, err := delegateAdd(ctx, baseDelegate, nil)
	if err != nil {
		return nil, err
	}

	for _, post := range conf.PostDelegates {
		next := deepCopyMap(post)
		if next == nil {
			continue
		}
		defaultName := pluginName
		if typ, _ := next["type"].(string); typ == "sbr" {
			defaultName = sbrDelegateName
		}
		next = configureDelegate(conf, masterName, next, defaultName)
		next["prevResult"] = convertResult(result, conf.CNIVersion)

		result, err = delegateAdd(ctx, next, nil)
		if err != nil {
			return nil, err
		}
	}

	return result, nil
}

func runDelegateDelChain(ctx context.Context, conf *pluginConf, masterName string) error {
	baseDelegate := deepCopyMap(conf.Delegate)
	if baseDelegate == nil {
		baseDelegate = map[string]any{}
	}
	baseDelegate = configureDelegate(conf, masterName, baseDelegate, ipvlanDelegateName)

	for i := len(conf.PostDelegates) - 1; i >= 0; i-- {
		post := deepCopyMap(conf.PostDelegates[i])
		if post == nil {
			continue
		}
		defaultName := pluginName
		if typ, _ := post["type"].(string); typ == "sbr" {
			defaultName = sbrDelegateName
		}
		post = configureDelegate(conf, masterName, post, defaultName)
		if err := delegateDel(ctx, post); err != nil {
			return err
		}
	}

	return delegateDel(ctx, baseDelegate)
}

func configureDelegate(conf *pluginConf, masterName string, delegate map[string]any, defaultName string) map[string]any {
	out := deepCopyMap(delegate)
	if out == nil {
		out = map[string]any{}
	}
	if _, ok := out["type"]; !ok {
		out["type"] = "ipvlan"
	}
	if _, ok := out["name"]; !ok {
		out["name"] = deriveDelegateName(conf, defaultName)
	}
	if masterName != "" {
		out["master"] = masterName
	}
	out["cniVersion"] = conf.CNIVersion
	return out
}

func deriveDelegateName(conf *pluginConf, defaultName string) string {
	if conf == nil {
		if defaultName != "" {
			return defaultName
		}
		return pluginName
	}
	if defaultName != "" {
		return defaultName
	}
	return pluginName
}

func delegateAdd(ctx context.Context, conf map[string]any, prev cnitypes.Result) (cnitypes.Result, error) {
	pluginType, _ := conf["type"].(string)
	if pluginType == "" {
		return nil, errors.New("delegate type is required")
	}
	if prev != nil {
		conf["prevResult"] = convertResult(prev, confVersion(conf))
	}

	netconf, err := json.Marshal(conf)
	if err != nil {
		return nil, fmt.Errorf("marshal delegate config: %w", err)
	}
	return invoke.DelegateAdd(ctx, pluginType, netconf, nil)
}

func delegateDel(ctx context.Context, conf map[string]any) error {
	pluginType, _ := conf["type"].(string)
	if pluginType == "" {
		return errors.New("delegate type is required")
	}
	netconf, err := json.Marshal(conf)
	if err != nil {
		return fmt.Errorf("marshal delegate config: %w", err)
	}
	return invoke.DelegateDel(ctx, pluginType, netconf, nil)
}

func confVersion(conf map[string]any) string {
	if v, ok := conf["cniVersion"].(string); ok && v != "" {
		return v
	}
	return version.Current()
}

func deepCopyMap(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	dst := make(map[string]any, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func convertResult(res cnitypes.Result, cniVersion string) *current.Result {
	cur, err := current.NewResultFromResult(res)
	if err != nil {
		return nil
	}
	cur.CNIVersion = cniVersion
	return cur
}

func collectInterfaceDetails(nsPath, ifName string, res cnitypes.Result) (*netns.InterfaceInfo, []net.IP, []net.IP, error) {
	cur, err := current.NewResultFromResult(res)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("parse result: %w", err)
	}

	info, err := netns.InspectInterface(nsPath, ifName)
	if err != nil {
		info = &netns.InterfaceInfo{Name: ifName}
	}

	ifaceIndex := -1
	for idx, iface := range cur.Interfaces {
		if iface.Name == ifName {
			ifaceIndex = idx
			break
		}
	}

	var ipv4 []net.IP
	var ipv6 []net.IP
	if ifaceIndex >= 0 {
		for _, ip := range cur.IPs {
			if ip.Interface != nil && *ip.Interface == ifaceIndex {
				if ip.Address.IP.To4() != nil {
					ipv4 = append(ipv4, ip.Address.IP)
				} else {
					ipv6 = append(ipv6, ip.Address.IP)
				}
			}
		}
	}

	if len(ipv4) == 0 && info != nil {
		ipv4 = append(ipv4, info.IPv4...)
	}
	if len(ipv6) == 0 && info != nil {
		ipv6 = append(ipv6, info.IPv6...)
	}

	return info, ipv4, filterIPv6Global(ipv6), nil
}

func filterIPv6Global(addrs []net.IP) []net.IP {
	out := make([]net.IP, 0, len(addrs))
	for _, ip := range addrs {
		if ip == nil {
			continue
		}
		if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			continue
		}
		out = append(out, ip)
	}
	return out
}

func writeCacheEntry(cacheDir string, entry cacheEntry) error {
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return fmt.Errorf("create cache dir: %w", err)
	}
	path := cacheFilePath(cacheDir, entry.PodUID, entry.InterfaceName, entry.ContainerID)
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal cache: %w", err)
	}
	return os.WriteFile(path, data, 0o600)
}

func readCacheEntry(cacheDir, containerID string) (cacheEntry, string, error) {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return cacheEntry{}, "", err
	}
	for _, entry := range entries {
		path := filepath.Join(cacheDir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return cacheEntry{}, "", err
		}
		var ce cacheEntry
		if err := json.Unmarshal(data, &ce); err != nil {
			return cacheEntry{}, "", err
		}
		if matchContainer(ce.ContainerID, containerID) {
			return ce, path, nil
		}
	}
	return cacheEntry{}, "", os.ErrNotExist
}

func cacheFilePath(cacheDir, podUID, ifName, containerID string) string {
	safePod := sanitizeComponent(podUID)
	safeIf := sanitizeComponent(ifName)
	safeID := truncateID(sanitizeComponent(containerID))
	name := fmt.Sprintf("%s_%s_%s.cache", safePod, safeIf, safeID)
	return filepath.Join(cacheDir, name)
}

func ipsToStrings(ips []net.IP) []string {
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, ip.String())
	}
	return out
}

func sanitizeComponent(component string) string {
	if component == "" {
		return "unknown"
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, component)
}

func truncateID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	if id == "" {
		return "unknown"
	}
	return id
}

func matchContainer(storedID, containerID string) bool {
	if storedID == "" {
		return false
	}
	short := truncateID(containerID)
	return truncateID(storedID) == short
}

func (c *pluginConf) waitTimeout() time.Duration {
	return time.Duration(c.WaitTimeoutSeconds) * time.Second
}

func (c *pluginConf) pollInterval() time.Duration {
	return time.Duration(c.PollIntervalMilliseconds) * time.Millisecond
}
