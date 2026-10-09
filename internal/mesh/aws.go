package mesh

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	awsRegion  = "eu-west-2"
	awsName    = "byk-compute-mesh-worker"
	awsRoot    = "/home/ubuntu/.local/share/compute-mesh-go/worker"
	awsCommand = "/home/ubuntu/.local/bin/switchyard"
)

//go:embed aws-bootstrap.sh
var awsBootstrap []byte

type awsTag struct {
	Key   string `json:"Key"`
	Value string `json:"Value"`
}

var awsTags = []awsTag{{Key: "Name", Value: awsName}, {Key: "Purpose", Value: "compute-mesh-single-worker"}}

type awsState struct {
	Account    string  `json:"account"`
	Region     string  `json:"region"`
	CreatedAt  float64 `json:"created_at"`
	InstanceID string  `json:"instance_id,omitempty"`
	GroupID    string  `json:"group_id,omitempty"`
	KeyName    string  `json:"key_name,omitempty"`
	ClientIP   string  `json:"client_ip,omitempty"`
}

type awsInstance struct {
	InstanceID      string                `json:"InstanceId"`
	PublicIPAddress string                `json:"PublicIpAddress"`
	State           struct{ Name string } `json:"State"`
	Tags            []awsTag              `json:"Tags"`
}

var (
	awsAccountPattern = regexp.MustCompile(`^[0-9]{12}$`)
	awsResourceID     = regexp.MustCompile(`^(i|sg|ami)-[0-9a-f]+$`)
	awsConsoleKey     = regexp.MustCompile(`cloud-init: 256 (SHA256:[A-Za-z0-9+/=]+) [^\n]+ \(ED25519\)`)
	awsCommandToken   = regexp.MustCompile(`^[A-Za-z0-9_./=-]+$`)
)

type awsDiagnostic struct{ data bytes.Buffer }

func (diagnostic *awsDiagnostic) Write(content []byte) (int, error) {
	if available := 8192 - diagnostic.data.Len(); available > 0 {
		_, _ = diagnostic.data.Write(content[:min(len(content), available)])
	}
	return len(content), nil
}

func awsDirectory() (string, error) {
	if override := os.Getenv("MESH_AWS_HOME"); override != "" {
		return filepath.Abs(override)
	}
	if legacy := os.Getenv("MESH_HOME"); legacy != "" {
		return filepath.Join(legacy, "aws-worker"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("could not locate the AWS worker state")
	}
	// Keep the Python worker's tracked keys and resources when moving to Go.
	return filepath.Join(home, ".local", "share", "compute-mesh", "aws-worker"), nil
}

func awsStatePath(dir string) string { return filepath.Join(dir, "state.json") }
func awsKeyPath(dir string) string   { return filepath.Join(dir, "worker-key") }
func awsKnownPath(dir string) string { return filepath.Join(dir, "known_hosts") }

func awsCall(parts ...string) (map[string]json.RawMessage, error) {
	if len(parts) < 2 {
		return nil, errors.New("invalid AWS operation")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	args := append(append([]string{}, parts...), "--region", awsRegion, "--output", "json")
	cmd := exec.CommandContext(ctx, "aws", args...)
	var output responseBuffer
	output.limit = 4 << 20
	cmd.Stdout = &output
	var diagnostic awsDiagnostic
	cmd.Stderr = &diagnostic
	started := time.Now()
	err := cmd.Run()
	if err != nil || output.tooLarge {
		debugEvent("aws-"+parts[0]+"-"+parts[1], "error", started)
		if ctx.Err() != nil {
			return nil, errors.New("AWS request timed out")
		}
		reason := "unknown AWS error"
		message := diagnostic.data.String()
		if strings.Contains(strings.ToLower(message), "expired") && strings.Contains(strings.ToLower(message), "login") {
			reason = "expired login"
		} else {
			for _, label := range []string{"ExpiredToken", "UnauthorizedOperation", "AccessDenied", "DependencyViolation", "InvalidInstanceID.NotFound", "InvalidGroup.NotFound", "InvalidKeyPair.NotFound", "InvalidPermission.Duplicate", "InvalidParameterValue", "InvalidParameterCombination", "InstanceLimitExceeded", "InsufficientInstanceCapacity", "VolumeInUse", "AuthFailure"} {
				if strings.Contains(message, label) {
					reason = label
					break
				}
			}
		}
		return nil, fmt.Errorf("AWS %s %s: %s", parts[0], parts[1], reason)
	}
	debugEvent("aws-"+parts[0]+"-"+parts[1], "ok", started)
	var result map[string]json.RawMessage
	if json.Unmarshal(output.data.Bytes(), &result) != nil {
		return nil, errors.New("AWS returned an invalid response")
	}
	return result, nil
}

func awsField[T any](data map[string]json.RawMessage, name string) (T, error) {
	var value T
	if json.Unmarshal(data[name], &value) != nil {
		return value, errors.New("AWS response is missing a required field")
	}
	return value, nil
}

func awsAccount() (string, error) {
	data, err := awsCall("sts", "get-caller-identity")
	if err != nil {
		return "", err
	}
	account, err := awsField[string](data, "Account")
	if err != nil || !awsAccountPattern.MatchString(account) {
		return "", errors.New("AWS identity is invalid")
	}
	return account, nil
}

func awsSave(dir string, state awsState) error {
	content, err := json.Marshal(state)
	if err != nil {
		return errors.New("could not encode AWS worker state")
	}
	if err := privateTree(dir); err != nil {
		return err
	}
	return writeAtomic(awsStatePath(dir), append(content, '\n'))
}

func awsLoad(dir string) (awsState, error) {
	content, err := privateRead(awsStatePath(dir), 16<<10)
	if err != nil {
		return awsState{}, errors.New("no valid tracked AWS worker; inspect local state before launching")
	}
	var state awsState
	if json.Unmarshal(content, &state) != nil || !awsAccountPattern.MatchString(state.Account) || state.Region != awsRegion || state.InstanceID != "" && !awsResourceID.MatchString(state.InstanceID) || state.GroupID != "" && !awsResourceID.MatchString(state.GroupID) {
		return awsState{}, errors.New("invalid AWS worker state")
	}
	account, err := awsAccount()
	if err != nil {
		return awsState{}, err
	}
	if state.Account != account {
		return awsState{}, errors.New("AWS worker belongs to a different account")
	}
	return state, nil
}

func hasAWSTags(tags []awsTag) bool {
	for _, expected := range awsTags {
		found := false
		for _, actual := range tags {
			if actual == expected {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func awsInstanceInfo(state awsState) (awsInstance, error) {
	if !awsResourceID.MatchString(state.InstanceID) || !strings.HasPrefix(state.InstanceID, "i-") {
		return awsInstance{}, errors.New("AWS worker launch is incomplete")
	}
	data, err := awsCall("ec2", "describe-instances", "--instance-ids", state.InstanceID)
	if err != nil {
		return awsInstance{}, err
	}
	reservations, err := awsField[[]struct{ Instances []awsInstance }](data, "Reservations")
	if err != nil {
		return awsInstance{}, err
	}
	var matches []awsInstance
	for _, group := range reservations {
		matches = append(matches, group.Instances...)
	}
	if len(matches) != 1 || matches[0].InstanceID != state.InstanceID || !hasAWSTags(matches[0].Tags) {
		return awsInstance{}, errors.New("tracked worker instance does not match its tag")
	}
	return matches[0], nil
}

func awsAddress(info awsInstance) (string, error) {
	ip := net.ParseIP(info.PublicIPAddress)
	if ip == nil || ip.To4() == nil || ip.String() != info.PublicIPAddress {
		return "", errors.New("worker has no reachable public IPv4 address yet")
	}
	return info.PublicIPAddress, nil
}

func awsPublicIP() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://checkip.amazonaws.com", nil)
	if err != nil {
		return "", errors.New("could not request the client's public IPv4 address")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", errors.New("could not determine the client's public IPv4 address")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", errors.New("could not determine the client's public IPv4 address")
	}
	content, err := io.ReadAll(io.LimitReader(response.Body, 65))
	if err != nil || len(content) > 64 {
		return "", errors.New("invalid client public IPv4 response")
	}
	ip := net.ParseIP(strings.TrimSpace(string(content)))
	if ip == nil || ip.To4() == nil {
		return "", errors.New("invalid client public IPv4 response")
	}
	return ip.String(), nil
}

func awsRefreshIngress(dir string, state *awsState) error {
	ip, err := awsPublicIP()
	if err != nil {
		return err
	}
	if !strings.HasPrefix(state.GroupID, "sg-") {
		return errors.New("AWS security group is not tracked")
	}
	data, err := awsCall("ec2", "describe-security-groups", "--group-ids", state.GroupID)
	if err != nil {
		return err
	}
	groups, err := awsField[[]struct {
		GroupID       string `json:"GroupId"`
		IpPermissions []struct {
			IpProtocol string `json:"IpProtocol"`
			FromPort   int    `json:"FromPort"`
			ToPort     int    `json:"ToPort"`
			IpRanges   []struct {
				CidrIP string `json:"CidrIp"`
			} `json:"IpRanges"`
		} `json:"IpPermissions"`
	}](data, "SecurityGroups")
	if err != nil || len(groups) != 1 || groups[0].GroupID != state.GroupID {
		return errors.New("tracked AWS security group is missing")
	}
	desired := ip + "/32"
	existing := make(map[string]bool)
	for _, permission := range groups[0].IpPermissions {
		if permission.IpProtocol == "tcp" && permission.FromPort == 22 && permission.ToPort == 22 {
			for _, item := range permission.IpRanges {
				if _, _, parseErr := net.ParseCIDR(item.CidrIP); parseErr != nil {
					return errors.New("invalid worker SSH ingress rule")
				}
				existing[item.CidrIP] = true
			}
		}
	}
	if !existing[desired] {
		if _, err := awsCall("ec2", "authorize-security-group-ingress", "--group-id", state.GroupID, "--protocol", "tcp", "--port", "22", "--cidr", desired); err != nil {
			return err
		}
	}
	for old := range existing {
		if old != desired {
			if _, err := awsCall("ec2", "revoke-security-group-ingress", "--group-id", state.GroupID, "--protocol", "tcp", "--port", "22", "--cidr", old); err != nil {
				return err
			}
		}
	}
	state.ClientIP = desired
	return awsSave(dir, *state)
}

func awsTagsJSON(resource string) string {
	content, _ := json.Marshal([]map[string]any{{"ResourceType": resource, "Tags": awsTags}})
	return string(content)
}

func awsSSHOptions(dir string) []string {
	return []string{"-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "ConnectTimeout=8",
		"-o", "StrictHostKeyChecking=yes", "-o", "ForwardAgent=no", "-o", "ClearAllForwardings=yes",
		"-o", "UserKnownHostsFile=" + awsKnownPath(dir), "-o", "HostKeyAlias=" + awsName,
		"-i", awsKeyPath(dir)}
}

func awsShellTokens(args ...string) (string, error) {
	for _, token := range args {
		if !awsCommandToken.MatchString(token) {
			return "", errors.New("invalid fixed worker setup command")
		}
	}
	return strings.Join(args, " "), nil
}

func awsSSH(dir, address string, args ...string) bool {
	return awsSSHWithin(dir, address, 30*time.Second, args...)
}

func awsSSHWithin(dir, address string, timeout time.Duration, args ...string) bool {
	remote, err := awsShellTokens(args...)
	if err != nil {
		return false
	}
	parameters := append(awsSSHOptions(dir), "ubuntu@"+address, remote)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", parameters...)
	cmd.Stdin = bytes.NewReader(nil)
	return cmd.Run() == nil
}

func awsConfigure(home, dir string, info awsInstance) error {
	ip, err := awsAddress(info)
	if err != nil {
		return err
	}
	return upsertAWSBackend(home, backendConfig{
		Name: "aws", Kind: "ssh", Root: awsRoot, Host: ip, User: "ubuntu",
		Identity: awsKeyPath(dir), KnownHosts: awsKnownPath(dir), HostKeyAlias: awsName,
		Command: awsCommand, Priority: 10, Capabilities: []string{"cpu"},
	})
}

func awsJSON(value any) string {
	content, _ := json.Marshal(value)
	return string(content)
}

func awsBool(value bool) string { return strconv.FormatBool(value) }

func awsLaunch(dir string) (awsState, error) {
	for _, path := range []string{awsStatePath(dir), awsKeyPath(dir), awsKnownPath(dir)} {
		if _, err := os.Lstat(path); err == nil || !os.IsNotExist(err) {
			return awsState{}, errors.New("existing AWS worker state must be resolved before launching")
		}
	}
	account, err := awsAccount()
	if err != nil {
		return awsState{}, err
	}
	instances, err := awsCall("ec2", "describe-instances", "--filters", "Name=tag:Name,Values="+awsName)
	if err != nil {
		return awsState{}, err
	}
	reservations, err := awsField[[]struct{ Instances []awsInstance }](instances, "Reservations")
	if err != nil {
		return awsState{}, err
	}
	for _, group := range reservations {
		for _, instance := range group.Instances {
			if instance.State.Name != "terminated" && instance.State.Name != "shutting-down" {
				return awsState{}, errors.New("a tagged worker already exists without local state")
			}
		}
	}
	keys, err := awsCall("ec2", "describe-key-pairs", "--filters", "Name=key-name,Values="+awsName)
	if err != nil {
		return awsState{}, err
	}
	keyPairs, err := awsField[[]json.RawMessage](keys, "KeyPairs")
	if err != nil || len(keyPairs) != 0 {
		return awsState{}, errors.New("a named AWS key pair already exists without local state")
	}
	vpcs, err := awsCall("ec2", "describe-vpcs", "--filters", "Name=is-default,Values=true")
	if err != nil {
		return awsState{}, err
	}
	networks, err := awsField[[]struct {
		VpcID string `json:"VpcId"`
	}](vpcs, "Vpcs")
	if err != nil || len(networks) != 1 || !strings.HasPrefix(networks[0].VpcID, "vpc-") {
		return awsState{}, errors.New("expected exactly one default London VPC")
	}
	subnets, err := awsCall("ec2", "describe-subnets", "--filters", "Name=vpc-id,Values="+networks[0].VpcID, "Name=default-for-az,Values=true")
	if err != nil {
		return awsState{}, err
	}
	choices, err := awsField[[]struct {
		SubnetID  string `json:"SubnetId"`
		Zone      string `json:"AvailabilityZone"`
		PublicIP  bool   `json:"MapPublicIpOnLaunch"`
		Available int    `json:"AvailableIpAddressCount"`
	}](subnets, "Subnets")
	if err != nil {
		return awsState{}, err
	}
	subnet := ""
	for _, choice := range choices {
		if choice.Zone == "eu-west-2a" && choice.PublicIP && choice.Available >= 8 {
			if subnet != "" {
				return awsState{}, errors.New("expected exactly one eligible default London subnet")
			}
			subnet = choice.SubnetID
		}
	}
	if !strings.HasPrefix(subnet, "subnet-") {
		return awsState{}, errors.New("expected exactly one eligible default London subnet")
	}
	parameter, err := awsCall("ssm", "get-parameter", "--name", "/aws/service/canonical/ubuntu/server/24.04/stable/current/amd64/hvm/ebs-gp3/ami-id")
	if err != nil {
		return awsState{}, err
	}
	ami, err := awsField[struct{ Value string }](parameter, "Parameter")
	if err != nil || !strings.HasPrefix(ami.Value, "ami-") {
		return awsState{}, errors.New("AWS Ubuntu AMI parameter is invalid")
	}
	images, err := awsCall("ec2", "describe-images", "--image-ids", ami.Value)
	if err != nil {
		return awsState{}, err
	}
	image, err := awsField[[]struct {
		Architecture   string
		OwnerID        string `json:"OwnerId"`
		RootDeviceName string
	}](images, "Images")
	if err != nil || len(image) != 1 || image[0].Architecture != "x86_64" || image[0].OwnerID != "099720109477" || image[0].RootDeviceName == "" {
		return awsState{}, errors.New("unexpected Canonical Ubuntu AMI")
	}
	if err := privateTree(dir); err != nil {
		return awsState{}, err
	}
	key := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", awsKeyPath(dir))
	if err := key.Run(); err != nil {
		return awsState{}, errors.New("could not create the worker SSH key")
	}
	state := awsState{Account: account, Region: awsRegion, CreatedAt: float64(time.Now().Unix())}
	if _, err := awsCall("ec2", "import-key-pair", "--key-name", awsName,
		"--public-key-material", "fileb://"+awsKeyPath(dir)+".pub", "--tag-specifications", awsTagsJSON("key-pair")); err != nil {
		// A request can succeed while losing its response. Retain tagged ownership.
		if found, checkErr := awsCall("ec2", "describe-key-pairs", "--filters", "Name=key-name,Values="+awsName); checkErr == nil {
			pairs, parseErr := awsField[[]struct{ Tags []awsTag }](found, "KeyPairs")
			if parseErr == nil && len(pairs) == 1 && hasAWSTags(pairs[0].Tags) {
				state.KeyName = awsName
				_ = awsSave(dir, state)
			}
		}
		if state.KeyName == "" {
			_ = os.Remove(awsKeyPath(dir))
			_ = os.Remove(awsKeyPath(dir) + ".pub")
		}
		return awsState{}, err
	}
	state.KeyName = awsName
	if err := awsSave(dir, state); err != nil {
		return awsState{}, err
	}
	group, err := awsCall("ec2", "create-security-group", "--group-name", awsName,
		"--description", "IP-restricted SSH for Compute Mesh worker", "--vpc-id", networks[0].VpcID,
		"--tag-specifications", awsTagsJSON("security-group"))
	if err != nil {
		return awsState{}, err
	}
	state.GroupID, err = awsField[string](group, "GroupId")
	if err != nil || !strings.HasPrefix(state.GroupID, "sg-") {
		return awsState{}, errors.New("created AWS security group has no valid ID; inspect tagged resources before retrying")
	}
	if err := awsSave(dir, state); err != nil {
		return awsState{}, err
	}
	if err := awsRefreshIngress(dir, &state); err != nil {
		return awsState{}, err
	}
	bootstrap, err := os.CreateTemp(dir, ".bootstrap-*")
	if err != nil {
		return awsState{}, errors.New("could not stage bounded worker bootstrap")
	}
	defer os.Remove(bootstrap.Name())
	if _, err := bootstrap.Write(awsBootstrap); err != nil {
		bootstrap.Close()
		return awsState{}, errors.New("could not stage bounded worker bootstrap")
	}
	if err := bootstrap.Close(); err != nil {
		return awsState{}, errors.New("could not finish bounded worker bootstrap")
	}
	interfaces := awsJSON([]map[string]any{{"DeviceIndex": 0, "SubnetId": subnet,
		"Groups": []string{state.GroupID}, "AssociatePublicIpAddress": true}})
	blockDevices := awsJSON([]map[string]any{{"DeviceName": image[0].RootDeviceName,
		"Ebs": map[string]any{"VolumeSize": 30, "VolumeType": "gp3", "Encrypted": true, "DeleteOnTermination": true}}})
	tagged := awsJSON([]map[string]any{{"ResourceType": "instance", "Tags": awsTags}, {"ResourceType": "volume", "Tags": awsTags}})
	launched, err := awsCall("ec2", "run-instances", "--image-id", ami.Value, "--instance-type", "m7i.xlarge",
		"--key-name", awsName, "--instance-initiated-shutdown-behavior", "stop",
		"--user-data", "file://"+bootstrap.Name(), "--metadata-options",
		"HttpEndpoint=enabled,HttpTokens=required,HttpPutResponseHopLimit=1,InstanceMetadataTags=disabled",
		"--network-interfaces", interfaces, "--block-device-mappings", blockDevices, "--tag-specifications", tagged)
	if err != nil {
		return awsState{}, err
	}
	created, err := awsField[[]awsInstance](launched, "Instances")
	if err != nil || len(created) != 1 || !strings.HasPrefix(created[0].InstanceID, "i-") {
		return awsState{}, errors.New("AWS worker launch result is invalid; inspect tagged resources before retrying")
	}
	state.InstanceID = created[0].InstanceID
	if err := awsSave(dir, state); err != nil {
		return awsState{}, err
	}
	return state, nil
}

func awsTrust(dir string, state awsState, info awsInstance) (bool, error) {
	ip, err := awsAddress(info)
	if err != nil {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	scan := exec.CommandContext(ctx, "ssh-keyscan", "-T", "8", "-t", "ed25519", ip)
	output, err := scan.Output()
	if err != nil || len(output) > 8192 {
		return false, nil
	}
	key := ""
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if strings.HasPrefix(line, ip+" ssh-ed25519 ") {
			if key != "" {
				return false, errors.New("worker reported more than one SSH host key")
			}
			key = line
		}
	}
	if key == "" {
		return false, nil
	}
	parts := strings.Fields(key)
	if len(parts) != 3 {
		return false, errors.New("worker SSH host key is invalid")
	}
	pinned := awsName + " " + parts[1] + " " + parts[2] + "\n"
	if _, err := os.Lstat(awsKnownPath(dir)); err == nil {
		previous, err := privateRead(awsKnownPath(dir), 8192)
		if err != nil || string(previous) != pinned {
			return false, errors.New("worker SSH host key changed; refusing to connect")
		}
		return true, nil
	} else if !os.IsNotExist(err) {
		return false, errors.New("could not inspect worker SSH host key")
	}
	console, err := awsCall("ec2", "get-console-output", "--instance-id", state.InstanceID, "--latest")
	if err != nil {
		return false, err
	}
	log, err := awsField[string](console, "Output")
	if err != nil {
		return false, nil
	}
	matches := awsConsoleKey.FindAllStringSubmatch(log, -1)
	if len(matches) != 1 {
		return false, nil
	}
	fingerprint := exec.Command("ssh-keygen", "-lf", "-", "-E", "sha256")
	fingerprint.Stdin = strings.NewReader(key + "\n")
	verified, err := fingerprint.Output()
	if err != nil || !strings.Contains(string(verified), matches[0][1]) {
		return false, errors.New("worker SSH key does not match its EC2 console fingerprint")
	}
	file, err := os.OpenFile(awsKnownPath(dir), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return false, errors.New("could not pin worker SSH host key")
	}
	if _, err := file.WriteString(pinned); err != nil {
		file.Close()
		return false, errors.New("could not pin worker SSH host key")
	}
	if err := file.Close(); err != nil {
		return false, errors.New("could not finish pinning worker SSH host key")
	}
	return true, nil
}

func awsUp(home, dir string) error {
	_, stateErr := os.Lstat(awsStatePath(dir))
	var state awsState
	var err error
	if os.IsNotExist(stateErr) {
		state, err = awsLaunch(dir)
	} else if stateErr == nil {
		state, err = awsLoad(dir)
	} else {
		return errors.New("could not inspect tracked AWS worker state")
	}
	if err != nil {
		return err
	}
	info, err := awsInstanceInfo(state)
	if err != nil {
		return err
	}
	switch info.State.Name {
	case "pending":
		fmt.Println("AWS worker launching; check switchyard aws status after it is running")
	case "stopped":
		if err := awsRefreshIngress(dir, &state); err != nil {
			return err
		}
		if _, err := awsCall("ec2", "start-instances", "--instance-ids", state.InstanceID); err != nil {
			return err
		}
		fmt.Println("AWS worker starting; check switchyard aws status before setup or run")
	case "running":
		if err := awsRefreshIngress(dir, &state); err != nil {
			return err
		}
		trusted, err := awsTrust(dir, state, info)
		if err != nil {
			return err
		}
		if trusted {
			if err := awsConfigure(home, dir, info); err != nil {
				return err
			}
			fmt.Println("AWS worker running; SSH key verified; check toolchain readiness")
		} else {
			fmt.Println("AWS worker running; SSH host key is not ready to pin yet")
		}
	default:
		return errors.New("AWS worker cannot start from its current state")
	}
	return nil
}

func awsStatus(home, dir string) error {
	if _, err := os.Lstat(awsStatePath(dir)); os.IsNotExist(err) {
		fmt.Println("No tracked AWS worker")
		return nil
	} else if err != nil {
		return errors.New("could not inspect AWS worker state")
	}
	state, err := awsLoad(dir)
	if err != nil {
		return err
	}
	if state.InstanceID == "" {
		fmt.Println("AWS worker launch incomplete; inspect tagged resources before retrying or destroying")
		return nil
	}
	info, err := awsInstanceInfo(state)
	if err != nil {
		return err
	}
	fmt.Printf("AWS London worker: %s; disk persists until switchyard aws destroy\n", info.State.Name)
	if info.State.Name != "running" {
		return nil
	}
	trusted, err := awsTrust(dir, state, info)
	if err != nil {
		return err
	}
	if !trusted {
		fmt.Println("SSH host key pending; check again after cloud-init starts")
		return nil
	}
	ip, err := awsAddress(info)
	if err != nil {
		return err
	}
	ready := awsSSH(dir, ip, "test", "-f", "/var/lib/compute-mesh/ready")
	installed := ready && awsSSH(dir, ip, "test", "-x", awsCommand)
	podman := ready && awsSSH(dir, ip, "podman", "info")
	docker := ready && !podman && awsSSH(dir, ip, "docker", "info")
	imageReady := installed && awsSSH(dir, ip, awsCommand, "image", "status")
	fmt.Printf("SSH host key verified; toolchain ready: %s; mesh installed: %s; rootless Podman ready: %s; legacy Docker ready: %s; pinned image ready: %s\n",
		awsBool(ready), awsBool(installed), awsBool(podman), awsBool(docker), awsBool(imageReady))
	if imageReady {
		return awsConfigure(home, dir, info)
	}
	return nil
}

func awsEnsurePodman(dir, ip string) error {
	if !awsSSH(dir, ip, "systemctl", "is-active", "--quiet", "compute-mesh-stop.timer") {
		return errors.New("worker auto-stop timer is not active; stop the worker before continuing")
	}
	if awsSSH(dir, ip, "podman", "info") && awsSSH(dir, ip, "test", "-x", "/usr/bin/catatonit") {
		return nil
	}
	bootstrap, err := os.CreateTemp(dir, ".bootstrap-*")
	if err != nil {
		return errors.New("could not stage rootless worker bootstrap")
	}
	defer os.Remove(bootstrap.Name())
	if _, err := bootstrap.Write(awsBootstrap); err != nil {
		bootstrap.Close()
		return errors.New("could not stage rootless worker bootstrap")
	}
	if err := bootstrap.Close(); err != nil {
		return errors.New("could not finish rootless worker bootstrap")
	}
	remote := "/home/ubuntu/mesh-bootstrap.next"
	params := append([]string{"-B"}, awsSSHOptions(dir)...)
	params = append(params, bootstrap.Name(), "ubuntu@"+ip+":"+remote)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if exec.CommandContext(ctx, "scp", params...).Run() != nil {
		return errors.New("could not transfer rootless worker bootstrap")
	}
	defer awsSSH(dir, ip, "rm", "-f", remote)
	if !awsSSHWithin(dir, ip, 10*time.Minute, "sudo", "-n", "/usr/bin/bash", remote) {
		return errors.New("rootless worker bootstrap failed; check the worker's cloud-init and package logs")
	}
	if !awsSSH(dir, ip, "podman", "info") || !awsSSH(dir, ip, "test", "-x", "/usr/bin/catatonit") ||
		!awsSSH(dir, ip, "systemctl", "is-active", "--quiet", "compute-mesh-stop.timer") {
		return errors.New("rootless Podman or the worker auto-stop timer is not ready")
	}
	return nil
}

func awsSetup(home, dir, source string) error {
	state, err := awsLoad(dir)
	if err != nil {
		return err
	}
	info, err := awsInstanceInfo(state)
	if err != nil {
		return err
	}
	if info.State.Name != "running" {
		return errors.New("start the AWS worker before setup")
	}
	trusted, err := awsTrust(dir, state, info)
	if err != nil {
		return err
	}
	if !trusted {
		return errors.New("worker SSH host key is not yet ready")
	}
	ip, err := awsAddress(info)
	if err != nil {
		return err
	}
	if !awsSSH(dir, ip, "test", "-f", "/var/lib/compute-mesh/ready") {
		return errors.New("worker toolchain is still bootstrapping")
	}
	if err := awsEnsurePodman(dir, ip); err != nil {
		return err
	}
	module, err := os.ReadFile(filepath.Join(source, "go.mod"))
	if err != nil || !strings.HasPrefix(string(module), "module github.com/BYK/switchyard\n") {
		return errors.New("--source must point to the Switchyard Go source tree")
	}
	build, err := os.MkdirTemp("", "switchyard-aws-*")
	if err != nil {
		return errors.New("could not prepare the worker binary build")
	}
	defer os.RemoveAll(build)
	binary := filepath.Join(build, "switchyard")
	cmd := exec.Command("go", "build", "-trimpath", "-o", binary, "./cmd/switchyard")
	cmd.Dir, cmd.Env = source, append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	started := time.Now()
	if err := cmd.Run(); err != nil {
		debugEvent("worker-build", "error", started)
		return errors.New("could not build the Go worker binary")
	}
	debugEvent("worker-build", "ok", started)
	params := append([]string{"-B"}, awsSSHOptions(dir)...)
	params = append(params, binary, "ubuntu@"+ip+":/home/ubuntu/switchyard.next")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "scp", params...).Run(); err != nil {
		return errors.New("could not transfer the Go worker binary")
	}
	for _, args := range [][]string{
		{"mkdir", "-p", "/home/ubuntu/.local/bin"},
		{"chmod", "755", "/home/ubuntu/switchyard.next"},
		{"mv", "-f", "/home/ubuntu/switchyard.next", awsCommand},
		{awsCommand, "worker", "--root", awsRoot, "init"},
	} {
		if !awsSSH(dir, ip, args...) {
			return errors.New("could not install the Go worker binary")
		}
	}
	if !awsSSH(dir, ip, "test", "-x", "/usr/bin/rsync") || !awsSSH(dir, ip, "podman", "info") {
		return errors.New("worker needs rsync and a working rootless Podman runtime")
	}
	imageBuildStart := time.Now()
	if !awsSSHWithin(dir, ip, 10*time.Minute, awsCommand, "image", "build") ||
		!awsSSH(dir, ip, awsCommand, "image", "status") {
		debugEvent("remote-image-build", "error", imageBuildStart)
		return errors.New("could not build and verify the pinned Node image on the worker")
	}
	debugEvent("remote-image-build", "ok", imageBuildStart)
	if err := awsConfigure(home, dir, info); err != nil {
		return err
	}
	if !awsSSH(dir, ip, "rm", "-f", "/home/ubuntu/.local/bin/mesh-go") {
		return errors.New("could not remove the old worker binary")
	}
	fmt.Println("Go worker installed; pinned SSH backend updated")
	return nil
}

func awsDown(dir string) error {
	state, err := awsLoad(dir)
	if err != nil {
		return err
	}
	info, err := awsInstanceInfo(state)
	if err != nil {
		return err
	}
	if info.State.Name == "stopped" {
		fmt.Println("AWS worker already stopped; EBS storage remains billable")
		return nil
	}
	if info.State.Name != "running" {
		return errors.New("AWS worker is not ready to stop")
	}
	if _, err := awsCall("ec2", "stop-instances", "--instance-ids", state.InstanceID); err != nil {
		return err
	}
	fmt.Println("AWS worker stop requested; verify with switchyard aws status")
	return nil
}

func awsDestroy(dir string) error {
	if _, err := os.Lstat(awsStatePath(dir)); os.IsNotExist(err) {
		fmt.Println("No tracked AWS worker to destroy")
		return nil
	} else if err != nil {
		return errors.New("could not inspect tracked AWS worker state")
	}
	state, err := awsLoad(dir)
	if err != nil {
		return err
	}
	if state.InstanceID != "" {
		info, err := awsInstanceInfo(state)
		if err != nil {
			if !strings.Contains(err.Error(), "InvalidInstanceID.NotFound") {
				return err
			}
			info.State.Name = "terminated"
		}
		if info.State.Name != "shutting-down" && info.State.Name != "terminated" {
			if _, err := awsCall("ec2", "terminate-instances", "--instance-ids", state.InstanceID); err != nil {
				return err
			}
			fmt.Println("AWS worker termination requested; check switchyard aws destroy again after termination")
			return nil
		}
		if info.State.Name != "terminated" {
			fmt.Println("AWS worker termination pending; check again after it completes")
			return nil
		}
		state.InstanceID = ""
		if err := awsSave(dir, state); err != nil {
			return err
		}
	}
	volumes, err := awsCall("ec2", "describe-volumes", "--filters", "Name=tag:Name,Values="+awsName)
	if err != nil {
		return err
	}
	disks, err := awsField[[]json.RawMessage](volumes, "Volumes")
	if err != nil {
		return err
	}
	if len(disks) != 0 {
		fmt.Println("Waiting for worker EBS volume deletion; check switchyard aws destroy after it completes")
		return nil
	}
	if state.GroupID != "" {
		if _, err := awsCall("ec2", "delete-security-group", "--group-id", state.GroupID); err != nil {
			return err
		}
		state.GroupID = ""
		if err := awsSave(dir, state); err != nil {
			return err
		}
	}
	if state.KeyName != "" {
		if state.KeyName != awsName {
			return errors.New("unrecognized tracked AWS key pair")
		}
		if _, err := awsCall("ec2", "delete-key-pair", "--key-name", state.KeyName); err != nil {
			return err
		}
		state.KeyName = ""
		if err := awsSave(dir, state); err != nil {
			return err
		}
	}
	for _, path := range []string{awsKeyPath(dir), awsKeyPath(dir) + ".pub", awsKnownPath(dir), awsStatePath(dir)} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return errors.New("could not remove local AWS worker material")
		}
	}
	fmt.Println("AWS worker, disk, security group, key pair and local SSH material removed")
	return nil
}

func runAWS(home string, args []string) int {
	if len(args) == 0 {
		return fail(errors.New("use aws up, setup, status, down or destroy"))
	}
	dir, err := awsDirectory()
	if err != nil {
		return fail(err)
	}
	source := "."
	if args[0] == "up" || args[0] == "setup" {
		if len(args) == 3 && args[1] == "--source" {
			source = args[2]
		} else if len(args) != 1 {
			return fail(errors.New("use --source ABSOLUTE_PROJECT_PATH"))
		}
	} else if len(args) != 1 {
		return fail(errors.New("AWS action takes no extra arguments"))
	}
	switch args[0] {
	case "up":
		err = awsUp(home, dir)
	case "setup":
		err = awsSetup(home, dir, source)
	case "status":
		err = awsStatus(home, dir)
	case "down":
		err = awsDown(dir)
	case "destroy":
		err = awsDestroy(dir)
	default:
		return fail(errors.New("use aws up, setup, status, down or destroy"))
	}
	if err != nil {
		return fail(err)
	}
	return 0
}
