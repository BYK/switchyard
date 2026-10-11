package switchyard

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Extra workers share the primary worker's tagged security group and SSH key.
// Their own tracked instance ID, launch token, slot tag and host-key pin keep
// each dispatch and cleanup bound to exactly one instance.
func awsLaunchPool(dir string) (awsState, error) {
	slot := awsSlot(dir)
	if slot < 2 || slot > 4 {
		return awsState{}, errors.New("invalid AWS pool slot")
	}
	base := awsPoolBase(dir)
	primary, err := awsLoad(base)
	if err != nil || primary.InstanceID == "" || primary.GroupID == "" || primary.KeyName != awsName {
		return awsState{}, errors.New("primary AWS worker must be tracked before scaling")
	}
	info, err := awsInstanceInfo(primary)
	if err != nil || info.State.Name != "running" || !strings.HasPrefix(info.SubnetID, "subnet-") || !strings.HasPrefix(info.ImageID, "ami-") {
		return awsState{}, errors.New("primary AWS worker is not ready to scale")
	}
	imageData, err := awsCall("ec2", "describe-images", "--image-ids", info.ImageID)
	if err != nil {
		return awsState{}, err
	}
	images, err := awsField[[]struct {
		Architecture   string
		OwnerID        string `json:"OwnerId"`
		RootDeviceName string
	}](imageData, "Images")
	if err != nil || len(images) != 1 || images[0].Architecture != "x86_64" || images[0].OwnerID != "099720109477" || images[0].RootDeviceName == "" {
		return awsState{}, errors.New("primary worker AMI is no longer available from Canonical")
	}
	subnetData, err := awsCall("ec2", "describe-subnets", "--subnet-ids", info.SubnetID)
	if err != nil {
		return awsState{}, err
	}
	subnets, err := awsField[[]struct {
		SubnetID  string `json:"SubnetId"`
		Zone      string `json:"AvailabilityZone"`
		Available int    `json:"AvailableIpAddressCount"`
	}](subnetData, "Subnets")
	if err != nil || len(subnets) != 1 || subnets[0].SubnetID != info.SubnetID || subnets[0].Zone != "eu-west-2a" || subnets[0].Available < 8 {
		return awsState{}, errors.New("primary worker subnet is not eligible for scaling")
	}
	var state awsState
	if _, err := os.Lstat(awsStatePath(dir)); os.IsNotExist(err) {
		for _, path := range []string{awsKeyPath(dir), awsKnownPath(dir)} {
			if _, err := os.Lstat(path); err == nil || !os.IsNotExist(err) {
				return awsState{}, errors.New("untracked pool SSH material exists")
			}
		}
		if err := privateTree(dir); err != nil {
			return awsState{}, err
		}
		key, err := privateRead(awsKeyPath(base), 8192)
		if err != nil {
			return awsState{}, errors.New("primary worker SSH key is not private")
		}
		if err := writeAtomic(awsKeyPath(dir), key); err != nil {
			return awsState{}, err
		}
		token, err := randomID()
		if err != nil {
			return awsState{}, err
		}
		state = awsState{Account: primary.Account, Region: awsRegion, CreatedAt: float64(time.Now().Unix()),
			GroupID: primary.GroupID, KeyName: awsName, Slot: slot, LaunchToken: token}
		if err := awsSave(dir, state); err != nil {
			return awsState{}, err
		}
	} else if err == nil {
		state, err = awsLoad(dir)
		if err != nil {
			return awsState{}, err
		}
	} else {
		return awsState{}, errors.New("could not inspect pool slot state")
	}
	if state.GroupID != primary.GroupID || state.KeyName != primary.KeyName || state.Account != primary.Account {
		return awsState{}, errors.New("pool slot does not match the primary worker")
	}
	if state.InstanceID != "" {
		return state, nil
	}
	found, err := awsFindPoolInstance(state)
	if err != nil {
		return awsState{}, err
	}
	if found != nil {
		state.InstanceID = found.InstanceID
		if err := awsSave(dir, state); err != nil {
			return awsState{}, err
		}
		return state, nil
	}
	if err := awsRefreshIngress(dir, &state); err != nil {
		return awsState{}, err
	}
	bootstrap, err := os.CreateTemp(dir, ".bootstrap-*")
	if err != nil {
		return awsState{}, errors.New("could not stage pool worker bootstrap")
	}
	defer os.Remove(bootstrap.Name())
	if _, err := bootstrap.Write(awsBootstrap); err != nil {
		bootstrap.Close()
		return awsState{}, errors.New("could not stage pool worker bootstrap")
	}
	if err := bootstrap.Close(); err != nil {
		return awsState{}, errors.New("could not finish pool worker bootstrap")
	}
	tags := append(append([]awsTag{}, awsTags...), awsTag{Key: "Slot", Value: strconv.Itoa(slot)}, awsTag{Key: "LaunchToken", Value: state.LaunchToken})
	interfaces := awsJSON([]map[string]any{{"DeviceIndex": 0, "SubnetId": info.SubnetID,
		"Groups": []string{primary.GroupID}, "AssociatePublicIpAddress": true}})
	volumes := awsJSON([]map[string]any{{"DeviceName": images[0].RootDeviceName,
		"Ebs": map[string]any{"VolumeSize": 30, "VolumeType": "gp3", "Encrypted": true, "DeleteOnTermination": true}}})
	tagged := awsJSON([]map[string]any{{"ResourceType": "instance", "Tags": tags}, {"ResourceType": "volume", "Tags": tags}})
	launched, err := awsCall("ec2", "run-instances", "--client-token", state.LaunchToken,
		"--image-id", info.ImageID, "--instance-type", "m7i.xlarge", "--key-name", awsName,
		"--instance-initiated-shutdown-behavior", "stop", "--user-data", "file://"+bootstrap.Name(),
		"--metadata-options", "HttpEndpoint=enabled,HttpTokens=required,HttpPutResponseHopLimit=1,InstanceMetadataTags=disabled",
		"--network-interfaces", interfaces, "--block-device-mappings", volumes, "--tag-specifications", tagged)
	if err != nil {
		return awsState{}, err
	}
	created, err := awsField[[]awsInstance](launched, "Instances")
	if err != nil || len(created) != 1 || !strings.HasPrefix(created[0].InstanceID, "i-") {
		return awsState{}, errors.New("AWS pool launch result is invalid; inspect the tagged slot")
	}
	state.InstanceID = created[0].InstanceID
	if err := awsSave(dir, state); err != nil {
		return awsState{}, err
	}
	return state, nil
}

// Find an interrupted launch by its private token. Never adopt an instance
// that merely reuses the pool's public slot tag.
func awsFindPoolInstance(state awsState) (*awsInstance, error) {
	filters := []string{"Name=tag:Name,Values=" + awsName, "Name=tag:Purpose,Values=switchyard-single-worker", "Name=tag:Slot,Values=" + strconv.Itoa(state.Slot)}
	found, err := awsCall("ec2", "describe-instances", "--filters", filters[0], filters[1], filters[2])
	if err != nil {
		return nil, err
	}
	groups, err := awsField[[]struct{ Instances []awsInstance }](found, "Reservations")
	if err != nil {
		return nil, err
	}
	var instances []awsInstance
	for _, group := range groups {
		for _, instance := range group.Instances {
			if instance.State.Name != "terminated" && instance.State.Name != "shutting-down" {
				instances = append(instances, instance)
			}
		}
	}
	if len(instances) > 1 {
		return nil, errors.New("multiple AWS instances claim the same pool slot")
	}
	if len(instances) == 1 {
		if !hasAWSTags(instances[0].Tags) || !hasAWSTagsFor(instances[0].Tags, []awsTag{{Key: "Slot", Value: strconv.Itoa(state.Slot)}, {Key: "LaunchToken", Value: state.LaunchToken}}) {
			return nil, errors.New("pool slot instance has invalid tags")
		}
		return &instances[0], nil
	}
	return nil, nil
}

func awsDestroyPool(home, base string) (bool, error) {
	for slot := 4; slot >= 2; slot-- {
		dir := awsPoolDir(base, slot)
		if _, err := os.Lstat(awsStatePath(dir)); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return false, errors.New("could not inspect tracked pool slot")
		}
		state, err := awsLoad(dir)
		if err != nil {
			return false, err
		}
		if state.InstanceID != "" {
			info, err := awsInstanceInfo(state)
			if err != nil {
				if !strings.Contains(err.Error(), "InvalidInstanceID.NotFound") {
					return false, err
				}
				info.State.Name = "terminated"
			}
			if info.State.Name != "terminated" && info.State.Name != "shutting-down" {
				return false, fmt.Errorf("stop and verify pool slot %d before destruction", slot)
			}
			if info.State.Name == "shutting-down" {
				return false, errors.New("pool worker termination is still pending")
			}
		}
		volumes, err := awsCall("ec2", "describe-volumes", "--filters", "Name=tag:Name,Values="+awsName, "Name=tag:Slot,Values="+strconv.Itoa(slot))
		if err != nil {
			return false, err
		}
		disks, err := awsField[[]any](volumes, "Volumes")
		if err != nil || len(disks) != 0 {
			return false, errors.New("pool worker EBS volume has not been deleted")
		}
		if err := removeDestroyedAWSBackend(home, dir); err != nil {
			return false, err
		}
		for _, path := range []string{awsStatePath(dir), awsKeyPath(dir), awsKnownPath(dir)} {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return false, errors.New("could not remove pool worker state")
			}
		}
	}
	return true, nil
}

func awsPoolStates(base string) ([]int, error) {
	var slots []int
	for slot := 2; slot <= 4; slot++ {
		_, err := os.Lstat(awsStatePath(awsPoolDir(base, slot)))
		if err == nil {
			slots = append(slots, slot)
		} else if !os.IsNotExist(err) {
			return nil, errors.New("could not inspect AWS pool state")
		}
	}
	return slots, nil
}

func awsPoolStatus(home, base string) error {
	slots, err := awsPoolStates(base)
	if err != nil {
		return err
	}
	for _, slot := range slots {
		fmt.Printf("Pool slot %d: ", slot)
		if err := awsStatus(home, awsPoolDir(base, slot)); err != nil {
			return err
		}
	}
	return nil
}

func awsPoolDestroy(home, base string) error {
	slots, err := awsPoolStates(base)
	if err != nil {
		return err
	}
	for _, slot := range slots {
		dir := awsPoolDir(base, slot)
		state, err := awsLoad(dir)
		if err != nil {
			return err
		}
		if state.InstanceID == "" {
			found, err := awsFindPoolInstance(state)
			if err != nil {
				return err
			}
			if found == nil {
				continue
			}
			state.InstanceID = found.InstanceID
			if err := awsSave(dir, state); err != nil {
				return err
			}
		}
		info, err := awsInstanceInfo(state)
		if err != nil {
			if !strings.Contains(err.Error(), "InvalidInstanceID.NotFound") {
				return err
			}
			// Terminated instances eventually disappear from DescribeInstances;
			// the volume check below still gates local state removal.
			continue
		}
		if info.State.Name != "terminated" && info.State.Name != "shutting-down" {
			if info.State.Name == "running" {
				if err := awsDown(dir); err != nil {
					return err
				}
				return errors.New("pool worker stopping; retry destroy after it is stopped")
			}
			if info.State.Name != "stopped" {
				return errors.New("pool worker is still changing state")
			}
			if _, err := awsCall("ec2", "terminate-instances", "--instance-ids", state.InstanceID); err != nil {
				return err
			}
			return errors.New("pool worker terminating; retry destroy after it is terminated")
		}
	}
	_, err = awsDestroyPool(home, base)
	return err
}

func awsPoolDown(base string) error {
	slots, err := awsPoolStates(base)
	if err != nil {
		return err
	}
	for _, slot := range slots {
		dir := awsPoolDir(base, slot)
		state, err := awsLoad(dir)
		if err != nil {
			return err
		}
		if state.InstanceID == "" {
			return errors.New("pool launch is incomplete")
		}
		info, err := awsInstanceInfo(state)
		if err != nil {
			return err
		}
		if info.State.Name == "running" {
			if err := awsDown(dir); err != nil {
				return err
			}
		} else if info.State.Name != "stopped" && info.State.Name != "stopping" {
			return errors.New("pool worker is still changing state")
		}
	}
	return nil
}
