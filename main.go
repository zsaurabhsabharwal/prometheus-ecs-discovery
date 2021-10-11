// Copyright 2017 Teralytics.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/awserr"
	"github.com/aws/aws-sdk-go-v2/aws/external"
	"github.com/aws/aws-sdk-go-v2/aws/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/go-yaml/yaml"
	"github.com/sirupsen/logrus"
)

type labels struct {
	TaskArn                    string            `yaml:"task_arn"`
	TaskName                   string            `yaml:"task_name"`
	JobName                    string            `yaml:"job,omitempty"`
	TaskRevision               string            `yaml:"task_revision"`
	TaskGroup                  string            `yaml:"task_group"`
	ClusterArn                 string            `yaml:"cluster_arn"`
	ContainerName              string            `yaml:"container_name"`
	ContainerArn               string            `yaml:"container_arn"`
	DockerImage                string            `yaml:"docker_image"`
	AvailabilityZone           string            `yaml:"availability_zone"`
	ContainerInstancePrivateIP string            `yaml:"container_instance_private_ip"`
	MetricsPath                string            `yaml:"__metrics_path__,omitempty"`
	CustomLabels               map[string]string `yaml:",inline,omitempty"`
}

// Docker label for enabling dynamic port detection
const dynamicPortLabel = "PROMETHEUS_DYNAMIC_EXPORT"

var verbosity = flag.Int("config.verbose", 0, "verbosity level for the application")
var cluster = flag.String("config.cluster", "", "name of the cluster to scrape")
var outDir = flag.String("config.write-to-dir", "/prometheus", "path of dir to write config file to")
var outFile = flag.String("config.write-to", "ecs_file_sd.yml", "name of file to write ECS service discovery information to")
var interval = flag.Duration("config.scrape-interval", 60*time.Second, "interval at which to scrape the AWS API for ECS service discovery information")
var times = flag.Int("config.scrape-times", 0, "how many times to scrape before exiting (0 = infinite)")
var roleArn = flag.String("config.role-arn", "", "ARN of the role to assume when scraping the AWS API (optional)")
var prometheusPortLabel = flag.String("config.port-label", "PROMETHEUS_EXPORTER_PORT", "Docker label to define the scrape port of the application (if missing an application won't be scraped), allowed comma separated ports")
var prometheusPathLabel = flag.String("config.path-label", "PROMETHEUS_EXPORTER_PATH", "Docker label to define the scrape path of the application")
var prometheusFilterLabel = flag.String("config.filter-label", "", "Docker label (and optionally value) to require to scrape the application")
var prometheusServerNameLabel = flag.String("config.server-name-label", "PROMETHEUS_EXPORTER_SERVER_NAME", "Docker label to define the server name")
var prometheusJobNameLabel = flag.String("config.job-name-label", "PROMETHEUS_EXPORTER_JOB_NAME", "Docker label to define the job name")
var prometheusDynamicPortDetection = flag.Bool("config.dynamic-port-detection", false, fmt.Sprintf("If true, only tasks with the Docker label %s=1 will be scraped", dynamicPortLabel))
var prometheusCustomLabelPrefix = flag.String("config.custom_label_prefix", "PROMETHEUS_EXPORTER_CUSTOM_LABEL_", "Prefix of custom docker labels")
var prometheusShardCountMapping = flagMapping{}

var log *logrus.Entry

// logError is a convenience function that decodes all possible ECS
// errors and displays them to standard error.
func logError(err error) {
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			// Print the error, cast err to awserr.Error to get the Code and
			// Message from an error.
			switch aerr.Code() {
			case ecs.ErrCodeServerException:
				log.WithFields(logrus.Fields{
					"err": ecs.ErrCodeServerException,
				}).Error(aerr.Error())
			case ecs.ErrCodeClientException:
				log.WithFields(logrus.Fields{
					"err": ecs.ErrCodeClientException,
				}).Error(aerr.Error())
			case ecs.ErrCodeInvalidParameterException:
				log.WithFields(logrus.Fields{
					"err": ecs.ErrCodeInvalidParameterException,
				}).Error(aerr.Error())
			case ecs.ErrCodeClusterNotFoundException:
				log.WithFields(logrus.Fields{
					"err": ecs.ErrCodeClusterNotFoundException,
				}).Error(aerr.Error())
			default:
				log.Error(aerr.Error())
			}
		} else {
			log.Error(err.Error())
		}
	}
}

// GetClusters retrieves a list of *ClusterArns from Amazon ECS,
// dealing with the mandatory pagination as needed.
func GetClusters(svc *ecs.ECS) (*ecs.ListClustersOutput, error) {
	input := &ecs.ListClustersInput{}
	output := &ecs.ListClustersOutput{}
	for {
		req := svc.ListClustersRequest(input)
		myoutput, err := req.Send()
		if err != nil {
			return nil, err
		}
		output.ClusterArns = append(output.ClusterArns, myoutput.ClusterArns...)
		if output.NextToken == nil {
			break
		}
		input.NextToken = output.NextToken
	}
	return output, nil
}

// AugmentedTask represents an ECS task augmented with an extra set of
// structures representing the ECS task definition and EC2 instance
// associated with the running task.
type AugmentedTask struct {
	*ecs.Task
	TaskDefinition *ecs.TaskDefinition
	EC2Instance    *ec2.Instance
}

// PrometheusContainer represents a tuple of information
// (Container Name, Container ARN, Docker image, Port)
// extracted from a task, its task definition
type PrometheusContainer struct {
	ContainerName string
	ContainerArn  string
	DockerImage   string
	Port          int
}

// PrometheusTaskInfo is the final structure that will be
// output as a Prometheus file service discovery config.
type PrometheusTaskInfo struct {
	Targets    []string `yaml:"targets"`
	Labels     labels   `yaml:"labels"`
	ConfigFile *string  `yaml:"-"`
}

// ExporterInformation returns a list of []*PrometheusTaskInfo
// enumerating the IPs, ports that the task's containers exports
// to Prometheus (one per container), so long as the Docker
// labels in its corresponding container definition for the
// container in the task has a PROMETHEUS_EXPORTER_PORT
//
// Example:
//     ...
//             "Name": "apache",
//             "DockerLabels": {
//                  "PROMETHEUS_EXPORTER_PORT": "1234"
//              },
//     ...
//              "PortMappings": [
//                {
//                  "ContainerPort": 1883,
//                  "HostPort": 0,
//                  "Protocol": "tcp"
//                },
//                {
//                  "ContainerPort": 1234,
//                  "HostPort": 0,
//                  "Protocol": "tcp"
//                }
//              ],
//     ...

func GetCustomLabel(v string) (string, string, error) {

	customLabel := strings.Split(v, ":")
	if len(customLabel) != 2 {
		return "", "", errors.New("incorrect custom label format")
	}

	return customLabel[0], customLabel[1], nil
}

func (t *AugmentedTask) ExporterInformation() []*PrometheusTaskInfo {
	logTask := log.WithFields(logrus.Fields{
		"taskGroup": *t.Group,
		"taskArn": *t.TaskArn,
	})

	ret := []*PrometheusTaskInfo{}

	var host string
	var ip string

	if t.LaunchType != ecs.LaunchTypeFargate {
		if t.EC2Instance == nil {
			logTask.Warn("task does not have ec2 instance information")
			return ret
		}
		if len(t.EC2Instance.NetworkInterfaces) == 0 {
			logTask.Warn("task ec2 instance does not have any assosciated network interfaces")
			return ret
		}

		logTask.WithFields(logrus.Fields{
			"ipaddress": *t.EC2Instance.PrivateIpAddress,
		}).Debug("host ec2 network ip address")
		ip = *t.EC2Instance.PrivateIpAddress

		if ip == "" {
			logTask.Warn("task ip is empty")
			return ret
		}
	}

	var filter []string
	if *prometheusFilterLabel != "" {
		filter = strings.Split(*prometheusFilterLabel, "=")
	}

	for _, i := range t.Containers {
		logContainer := logTask.WithFields(logrus.Fields{
			"container": *i.Name,
		})
		// Let's go over the containers to see which ones are defined
		var d ecs.ContainerDefinition
		for _, d = range t.TaskDefinition.ContainerDefinitions {
			if *i.Name == *d.Name {
				// Aha, the container definition match this container we
				// are inspecting, stop the loop cos we got the D now.
				break
			}
		}
		if *i.Name != *d.Name && t.LaunchType != ecs.LaunchTypeFargate {
			// Nope, no match, this container cannot be exported.  We continue.
			logContainer.Debug("container did not match")
			continue
		}

		var hostPorts []int64
		if *prometheusDynamicPortDetection {
			logContainer.Trace("prometheus dynamic port detection enabled")
			v, ok := d.DockerLabels[dynamicPortLabel]
			if !ok || v != "1" {
				// Nope, no Prometheus-exported port in this container def.
				// This container is no good. We continue.
				logContainer.Debug("container did not export prometheus port")
				continue
			}

			if len(i.NetworkBindings) != 1 {
				// Dynamic port mapping is only supported with a single binding.
				// Otherwise, how would we know which port to use?
				continue
			}

			if port := i.NetworkBindings[0].HostPort; port != nil {
				hostPorts = append(hostPorts, *port)
			}
		} else {
			v, ok := d.DockerLabels[*prometheusPortLabel]
			if !ok {
				// Nope, no Prometheus-exported port in this container def.
				// This container is no good.  We continue.
				logContainer.Debug("container did not export prometheus port")
				continue
			}

			if len(filter) != 0 {
				v, ok := d.DockerLabels[filter[0]]
				if !ok {
					// Nope, the filter label isn't present.
					logContainer.WithFields(logrus.Fields{
						"label": filter[0],
					}).Debug("container filter label not found")
					continue
				}
				if len(filter) == 2 && v != filter[1] {
					// Nope, the filter label value doesn't match.
					logContainer.WithFields(logrus.Fields{
						"label": filter[0],
					}).Debug("container filter label value does not match")
					continue
				}
			}

			var exporterPorts []int64
			var err error

			portsArr := strings.Split(v, ",")
			for _, p := range portsArr {
				var pInt int
				pInt, err = strconv.Atoi(p)
				if err != nil {
					break
				} else if pInt >= 0 {
					exporterPorts = append(exporterPorts, int64(pInt))
				}
			}

			if err != nil || len(exporterPorts) == 0 {
				// This container has an invalid port definition.
				// This container is no good.  We continue.
				logContainer.Debug("container has no or invalid exported ports")
				continue
			}

			if len(i.NetworkBindings) > 0 {
				for _, nb := range i.NetworkBindings {
					if _, found := Find(exporterPorts, *nb.ContainerPort); found {
						hostPorts = append(hostPorts, *nb.HostPort)
					}
				}
			} else {
				for _, ni := range i.NetworkInterfaces {
					if *ni.PrivateIpv4Address != "" {
						ip = *ni.PrivateIpv4Address
					}
				}
				hostPorts = append(hostPorts, exporterPorts...)
			}
		}

		var exporterServerName string
		var exporterPath string
		var ok bool
		exporterServerName, ok = d.DockerLabels[*prometheusServerNameLabel]
		if ok {
			host = strings.TrimRight(exporterServerName, "/")
		} else {
			// No server name, so fall back to ip address
			host = ip
		}

		configFile := outFile
		m := make(map[string]string)
		for k, v := range d.DockerLabels {
			if strings.HasPrefix(k, *prometheusCustomLabelPrefix) {
				key, val, err := GetCustomLabel(v)
				if err == nil {
					m[key] = val
				} else {
					errCustomLabel := errors.New("Skipping label " + k + " in task " + *t.TaskDefinition.Family + " of cluster " + *t.ClusterArn + " : " + err.Error())
					logError(errCustomLabel)
				}
			}

			if k == "PROMETHEUS_SHARD_IDENTIFIER" {
				filename := ""
				// if the shard count mapping is defined create that many shard files
				if count, ok := prometheusShardCountMapping[v]; ok {
					filename = fmt.Sprintf("%s_shard_%d.yml", v, hash(*t.TaskArn, count))
				} else {
					filename = fmt.Sprintf("%s.yml", v)
				}
				configFile = &filename
			}
		}

		containerInstancePrivateIpAddress := ""
		if t.EC2Instance != nil && t.EC2Instance.PrivateIpAddress != nil {
			containerInstancePrivateIpAddress = *t.EC2Instance.PrivateIpAddress
		}

		availabilityZone := ""
		if t.EC2Instance != nil && t.EC2Instance.Placement != nil && t.EC2Instance.Placement.AvailabilityZone != nil {
			availabilityZone = *t.EC2Instance.Placement.AvailabilityZone
		}

		l := labels{
			TaskArn:                    *t.TaskArn,
			TaskName:                   *t.TaskDefinition.Family,
			JobName:                    d.DockerLabels[*prometheusJobNameLabel],
			TaskRevision:               fmt.Sprintf("%d", *t.TaskDefinition.Revision),
			TaskGroup:                  *t.Group,
			ClusterArn:                 *t.ClusterArn,
			ContainerName:              *i.Name,
			ContainerArn:               *i.ContainerArn,
			DockerImage:                *d.Image,
			ContainerInstancePrivateIP: containerInstancePrivateIpAddress,
			AvailabilityZone:           availabilityZone,
			CustomLabels:               m,
		}

		exporterPath, ok = d.DockerLabels[*prometheusPathLabel]
		if ok {
			l.MetricsPath = exporterPath
		}

		var targets []string
		for _, p := range hostPorts {
			targets = append(targets, fmt.Sprintf("%s:%d", host, p))
		}

		ret = append(ret, &PrometheusTaskInfo{
			Targets:    targets,
			Labels:     l,
			ConfigFile: configFile,
		})

		logContainer.WithFields(logrus.Fields{
			"meta": ret,
		}).Trace("container meta information")
	}

	return ret
}

// AddTaskDefinitionsOfTasks adds to each Task the TaskDefinition
// corresponding to it.
func AddTaskDefinitionsOfTasks(svc *ecs.ECS, taskList []*AugmentedTask) ([]*AugmentedTask, error) {
	task2def := make(map[string]*ecs.TaskDefinition)
	for _, task := range taskList {
		task2def[*task.TaskDefinitionArn] = nil
	}

	jobs := make(chan *ecs.DescribeTaskDefinitionInput, len(task2def))
	results := make(chan struct {
		out *ecs.DescribeTaskDefinitionOutput
		err error
	}, len(task2def))

	for w := 1; w <= 4; w++ {
		go func() {
			for in := range jobs {
				req := svc.DescribeTaskDefinitionRequest(in)
				out, err := req.Send()
				results <- struct {
					out *ecs.DescribeTaskDefinitionOutput
					err error
				}{out, err}
			}
		}()
	}

	for tn := range task2def {
		m := string(append([]byte{}, tn...))
		jobs <- &ecs.DescribeTaskDefinitionInput{TaskDefinition: &m}
	}
	close(jobs)

	var err error
	for range task2def {
		result := <-results
		if result.err != nil {
			err = result.err
			log.Errorf("Error describing task definition: %s", err)
		} else {
			task2def[*result.out.TaskDefinition.TaskDefinitionArn] = result.out.TaskDefinition
		}
	}
	if err != nil {
		return nil, err
	}

	for _, task := range taskList {
		task.TaskDefinition = task2def[*task.TaskDefinitionArn]
	}
	return taskList, nil
}

// StringToStarString converts a list of strings to a list of
// pointers to strings, which is a common requirement of the
// Amazon API.
func StringToStarString(s []string) []*string {
	c := make([]*string, 0, len(s))
	for n := range s {
		c = append(c, &s[n])
	}
	return c
}

// SplitArray splits given array into chunks, it's useful
// because AWS API has limits on number of elements you can
// submit via one call.
func SplitArray(a []string, size int) [][]string {
	var splitted [][]string
	for i := 0; i < len(a); i += size {
		end := i + size
		if end > len(a) {
			end = len(a)
		}
		splitted = append(splitted, a[i:end])
	}
	return splitted
}

// DescribeInstancesUnpaginated describes a list of EC2 instances.
// It is unpaginated because the API function does not require
// pagination.
func DescribeInstancesUnpaginated(svc *ec2.EC2, instanceIds []string) ([]ec2.Instance, error) {
	if len(instanceIds) == 0 {
		return nil, nil
	}
	finalOutput := &ec2.DescribeInstancesOutput{}
	splittedInstanceIds := SplitArray(instanceIds, 100)
	for _, chunkedInstanceIds := range splittedInstanceIds {
		input := &ec2.DescribeInstancesInput{
			InstanceIds: chunkedInstanceIds,
		}
		for {
			req := svc.DescribeInstancesRequest(input)
			output, err := req.Send()
			if err != nil {
				return nil, err
			}
			log.Infof("Described %d EC2 reservations", len(output.Reservations))
			finalOutput.Reservations = append(finalOutput.Reservations, output.Reservations...)
			if output.NextToken == nil {
				break
			}
			input.NextToken = output.NextToken
		}
	}
	result := []ec2.Instance{}
	for _, rsv := range finalOutput.Reservations {
		result = append(result, rsv.Instances...)
	}
	return result, nil
}

// AddContainerInstancesToTasks adds to each Task the EC2 instance
// running its containers.
func AddContainerInstancesToTasks(svc *ecs.ECS, svcec2 *ec2.EC2, taskList []*AugmentedTask) ([]*AugmentedTask, error) {

	clusterArnToContainerInstancesArns := make(map[string]map[string]*ecs.ContainerInstance)
	for _, task := range taskList {
		if task.ContainerInstanceArn != nil {
			if _, ok := clusterArnToContainerInstancesArns[*task.ClusterArn]; !ok {
				clusterArnToContainerInstancesArns[*task.ClusterArn] = make(map[string]*ecs.ContainerInstance)
			}
			clusterArnToContainerInstancesArns[*task.ClusterArn][*task.ContainerInstanceArn] = nil
		}
	}

	instanceIDToEC2Instance := make(map[string]*ec2.Instance)
	for clusterArn, containerInstancesArns := range clusterArnToContainerInstancesArns {
		keys := make([]string, 0, len(containerInstancesArns))
		for k := range containerInstancesArns {
			keys = append(keys, k)
		}

		splittedKeys := SplitArray(keys, 100)
		for _, chunkedKeys := range splittedKeys {
			input := &ecs.DescribeContainerInstancesInput{
				Cluster:            &clusterArn,
				ContainerInstances: chunkedKeys,
			}
			req := svc.DescribeContainerInstancesRequest(input)
			output, err := req.Send()
			if err != nil {
				return nil, err
			}

			if len(output.Failures) > 0 {
				log.Infof("Described %d failures in cluster %s", len(output.Failures), clusterArn)
			}
			for _, ci := range output.ContainerInstances {
				cInst := ci
				clusterArnToContainerInstancesArns[clusterArn][*cInst.ContainerInstanceArn] = &cInst
				instanceIDToEC2Instance[*cInst.Ec2InstanceId] = nil
			}
		}
	}
	if len(instanceIDToEC2Instance) == 0 {
		return taskList, nil
	}

	keys := make([]string, 0, len(instanceIDToEC2Instance))
	for id := range instanceIDToEC2Instance {
		keys = append(keys, id)
	}

	instances, err := DescribeInstancesUnpaginated(svcec2, keys)
	if err != nil {
		return nil, err
	}

	for _, i := range instances {
		inst := i
		instanceIDToEC2Instance[*i.InstanceId] = &inst
	}

	for _, task := range taskList {
		if task.ContainerInstanceArn != nil {
			containerInstance, ok := clusterArnToContainerInstancesArns[*task.ClusterArn][*task.ContainerInstanceArn]
			if !ok {
				log.Infof("Cannot find container instance %s in cluster %s", *task.ContainerInstanceArn, *task.ClusterArn)
				continue
			}
			instance, ok := instanceIDToEC2Instance[*containerInstance.Ec2InstanceId]
			if !ok {
				log.Infof("Cannot find EC2 instance %s", *containerInstance.Ec2InstanceId)
				continue
			}
			task.EC2Instance = instance
		}
	}

	return taskList, nil
}

// GetTasksOfClusters returns the EC2 tasks running in a list of Clusters.
func GetTasksOfClusters(svc *ecs.ECS, svcec2 *ec2.EC2, clusterArns []*string) ([]ecs.Task, error) {
	jobs := make(chan *string, len(clusterArns))
	results := make(chan struct {
		out *ecs.DescribeTasksOutput
		err error
	}, len(clusterArns))

	for w := 1; w <= 4; w++ {
		go func() {
			for clusterArn := range jobs {
				input := &ecs.ListTasksInput{
					Cluster: clusterArn,
				}
				finalOutput := &ecs.DescribeTasksOutput{}
				var err error
				for {
					req := svc.ListTasksRequest(input)
					output, err1 := req.Send()
					if err1 != nil {
						err = err1
						log.Errorf("Error listing tasks of cluster %s: %s", *clusterArn, err)
						break
					}
					if len(output.TaskArns) == 0 {
						break
					}
					log.Infof("Inspected cluster %s, found %d tasks", *clusterArn, len(output.TaskArns))
					reqDescribe := svc.DescribeTasksRequest(&ecs.DescribeTasksInput{
						Cluster: clusterArn,
						Tasks:   output.TaskArns,
					})
					descOutput, err2 := reqDescribe.Send()
					if err2 != nil {
						err = err2
						log.Errorf("Error describing tasks of cluster %s: %s", *clusterArn, err)
						break
					}
					log.Infof("Described %d tasks in cluster %s", len(descOutput.Tasks), *clusterArn)
					if len(descOutput.Failures) > 0 {
						log.Errorf("Described %d failures in cluster %s", len(descOutput.Failures), *clusterArn)
					}
					finalOutput.Tasks = append(finalOutput.Tasks, descOutput.Tasks...)
					finalOutput.Failures = append(finalOutput.Failures, descOutput.Failures...)
					if output.NextToken == nil {
						break
					}
					input.NextToken = output.NextToken
				}
				results <- struct {
					out *ecs.DescribeTasksOutput
					err error
				}{finalOutput, err}
			}
		}()
	}

	for _, clusterArn := range clusterArns {
		jobs <- clusterArn
	}
	close(jobs)

	tasks := []ecs.Task{}
	for range clusterArns {
		result := <-results
		if result.err != nil {
			return nil, result.err
		}
		tasks = append(tasks, result.out.Tasks...)
	}

	return tasks, nil
}

// GetAugmentedTasks gets the fully AugmentedTasks running on
// a list of Clusters.
func GetAugmentedTasks(svc *ecs.ECS, svcec2 *ec2.EC2, clusterArns []*string) ([]*AugmentedTask, error) {
	simpleTasks, err := GetTasksOfClusters(svc, svcec2, clusterArns)
	if err != nil {
		return nil, err
	}

	tasks := []*AugmentedTask{}
	for i := 0; i < len(simpleTasks); i++ {
		tasks = append(tasks, &AugmentedTask{&simpleTasks[i], nil, nil})
	}
	tasks, err = AddTaskDefinitionsOfTasks(svc, tasks)
	if err != nil {
		return nil, err
	}

	tasks, err = AddContainerInstancesToTasks(svc, svcec2, tasks)
	if err != nil {
		return nil, err
	}

	return tasks, nil
}

func main() {
	flag.Var(&prometheusShardCountMapping, "shard-count", "provide a shard count mapping ex: -shard-count <shard_name>=<shard_count>")
	flag.Parse()

	// DISC_CONFIG_ROLE-ARN - arn:aws:iam::xxxxxxxxxxx:role/role-name
	accountID := (strings.Split(*roleArn, ":"))[4]

	logger := logrus.New()
	logger.SetFormatter(&logrus.JSONFormatter{})
	logger.SetOutput(os.Stdout)
	logger.SetLevel(logrus.Level(*verbosity + 2)) // Adding +2 here to skip panic and fatal level in logrus library

	log = logger.WithFields(logrus.Fields{
		"application": "prometheus-ecs-discovery",
		"region": os.Getenv("AWS_REGION"),
		"accountID": accountID,
	})

	logger.Info("starting ecs discovery...")

	config, err := external.LoadDefaultAWSConfig()
	if err != nil {
		logError(err)
		return
	}

	if *roleArn != "" {
		// Assume role
		stsSvc := sts.New(config)
		config.Credentials = stscreds.NewAssumeRoleProvider(stsSvc, *roleArn)
	}

	// Initialise AWS Service clients
	svc := ecs.New(config)
	svcec2 := ec2.New(config)

	work := func() {
		var clusters *ecs.ListClustersOutput

		if *cluster != "" {
			res, err := svc.DescribeClustersRequest(&ecs.DescribeClustersInput{
				Clusters: []string{*cluster},
			}).Send()
			if err != nil {
				logError(err)
				return
			}

			if len(res.Clusters) == 0 {
				logError(fmt.Errorf(
					"%s cluster not found",
					ecs.ErrCodeClusterNotFoundException,
				))
				return
			}

			clusters = &ecs.ListClustersOutput{
				ClusterArns: []string{*cluster},
			}
		} else {
			c, err := GetClusters(svc)
			if err != nil {
				logError(err)
				return
			}
			clusters = c
		}

		tasks, err := GetAugmentedTasks(svc, svcec2, StringToStarString(clusters.ClusterArns))
		if err != nil {
			logError(err)
			return
		}

		// Get mapping of output filename and content
		filenameInfosMapping := map[string][]*PrometheusTaskInfo{}

		for _, t := range tasks {
			logger.WithFields(logrus.Fields{
				"taskObj": *t,
			}).Trace("exporting task information")
			infoList := t.ExporterInformation()

			for _, info := range infoList {
				if _, ok := filenameInfosMapping[*info.ConfigFile]; !ok {
					filenameInfosMapping[*info.ConfigFile] = []*PrometheusTaskInfo{}
				}

				filenameInfosMapping[*info.ConfigFile] = append(filenameInfosMapping[*info.ConfigFile], info)
			}
		}

		if len(filenameInfosMapping) == 0 {
			logger.Trace("no filename info mapping found")
		}
		// Write content in the respective files
		for configFile, content := range filenameInfosMapping {
			m, err := yaml.Marshal(content)
			if err != nil {
				logError(err)
				return
			}
			log.Infof("Writing %d discovered exporters to %s", len(content), configFile)
			err = ioutil.WriteFile(strings.TrimRight(*outDir, "/")+"/"+strings.TrimLeft(configFile, "/"), m, 0644)
			if err != nil {
				logError(err)
				return
			}
		}

	}
	s := time.NewTimer(1 * time.Millisecond)
	t := time.NewTicker(*interval)
	n := *times
	for {
		select {
		case <-s.C:
		case <-t.C:
		}
		work()
		n = n - 1
		if *times > 0 && n == 0 {
			break
		}
	}
}
