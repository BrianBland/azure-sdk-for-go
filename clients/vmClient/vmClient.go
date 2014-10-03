package vmClient

import (
	"fmt"
	"time"
	"encoding/xml"
	"encoding/base64"
	"encoding/pem"
	"os"
	"io/ioutil"
	"crypto/rand"
	"crypto/sha1"
	"io"
	"errors"
	"strings"
	"os/user"
	"path"
	"github.com/MSOpenTech/azure-sdk-for-go/clients/locationClient"
	"github.com/MSOpenTech/azure-sdk-for-go/clients/imageClient"
	"github.com/MSOpenTech/azure-sdk-for-go/clients/storageServiceClient"
	azure "github.com/MSOpenTech/azure-sdk-for-go"
)

const (
	azureXmlns = "http://schemas.microsoft.com/windowsazure"
	azureDeploymentListURL = "services/hostedservices/%s/deployments"
	azureHostedServiceListURL = "services/hostedservices"
	azureHostedServiceURL = "services/hostedservices/%s"
	azureDeploymentURL = "services/hostedservices/%s/deployments/%s"
	azureRoleURL = "services/hostedservices/%s/deployments/%s/roles/%s"
	azureOperationsURL = "services/hostedservices/%s/deployments/%s/roleinstances/%s/Operations"
	azureCertificatListURL = "services/hostedservices/%s/certificates"

	osLinux = "Linux"
	osWindows = "Windows"

	dockerPublicConfig = "{ \"dockerport\": \"%v\" }"
	dockerPrivateConfig = "{ \"ca\": \"%s\", \"server-cert\": \"%s\", \"server-key\": \"%s\" }"
	dockerDirExistsMessage = "Docker directory exists"

	missingDockerCertsError = "You should generate docker certificates first. Info can be found here: https://docs.docker.com/articles/https/"
	provisioningConfDoesNotExistsError = "You should set azure VM provisioning config first"
	invalidCertExtensionError = "Certificate %s is invalid. Please specify %s certificate."
	invalidOSError = "You must specify correct OS param. Valid values are 'Linux' and 'Windows'"
)

// REGION PUBLIC METHODS STARTS

func CreateAzureVM(role *Role, dnsName, location string) error {

	err := locationClient.ResolveLocation(location)
	if err != nil {
		return err
	}

	fmt.Println("Creating hosted service... ")
	requestId, err := CreateHostedService(dnsName, location)
	if err != nil {
		return err
	}

	azure.WaitAsyncOperation(requestId)

	if role.UseCertAuth {
		fmt.Println("Uploading cert...")

		err = uploadServiceCert(dnsName, role.CertPath)
		if err != nil {
			return err
		}
	}

	fmt.Println("Deploying azure VM configuration... ")

	vMDeployment := createVMDeploymentConfig(role)
	vMDeploymentBytes, err := xml.Marshal(vMDeployment)
	if err != nil {
		return err
	}

	requestURL :=  fmt.Sprintf(azureDeploymentListURL, role.RoleName)
	requestId, err = azure.SendAzurePostRequest(requestURL, vMDeploymentBytes)
	if err != nil {
		return err
	}

	azure.WaitAsyncOperation(requestId)

	return nil
}

func CreateHostedService(dnsName, location string) (string, error) {

	err := locationClient.ResolveLocation(location)
	if err != nil {
		return "", err
	}

	hostedServiceDeployment := createHostedServiceDeploymentConfig(dnsName, location)
	hostedServiceBytes, err := xml.Marshal(hostedServiceDeployment)
	if err != nil {
		return "", err
	}

	requestURL := azureHostedServiceListURL
	requestId, azureErr := azure.SendAzurePostRequest(requestURL, hostedServiceBytes)
	if azureErr != nil {
		return "", err
	}

	return requestId, nil
}

func DeleteHostedService(dnsName string) error {

	requestURL := fmt.Sprintf(azureHostedServiceURL, dnsName)
	requestId, err := azure.SendAzureDeleteRequest(requestURL)
	if err != nil {
		return err
	}

	azure.WaitAsyncOperation(requestId)
	return nil
}

func CreateAzureVMConfiguration(name, instanceSize, imageName, location string) (*Role, error) {
	fmt.Println("Creating azure VM configuration... ")

	err := locationClient.ResolveLocation(location)
	if err != nil {
		return nil, err
	}

	role, err := createAzureVMRole(name, instanceSize, imageName, location)
	if err != nil {
		return nil, err
	}

	return role, nil
}

func AddAzureLinuxProvisioningConfig(azureVMConfig *Role, userName, password, certPath string) (*Role, error) {
	fmt.Println("Adding azure provisioning configuration... ")

	configurationSets := ConfigurationSets{}

	provisioningConfig, err := createLinuxProvisioningConfig(azureVMConfig.RoleName, userName, password, certPath)
	if err != nil {
		return nil, err
	}

	configurationSets.ConfigurationSet = append(configurationSets.ConfigurationSet, provisioningConfig)

	networkConfig, networkErr := createNetworkConfig(osLinux)
	if networkErr != nil {
		return nil, err
	}

	configurationSets.ConfigurationSet = append(configurationSets.ConfigurationSet, networkConfig)

	azureVMConfig.ConfigurationSets = configurationSets

	if len(certPath) > 0 {
		azureVMConfig.UseCertAuth = true
		azureVMConfig.CertPath = certPath
	}

	return azureVMConfig, nil
}

func SetAzureVMExtension(azureVMConfiguration *Role, name string, publisher string, version string, referenceName string, state string, publicConfigurationValue string, privateConfigurationValue string) (*Role) {
	fmt.Printf("Setting azure VM extension: %s... \n", name)

	extension := ResourceExtensionReference{}
	extension.Name = name
	extension.Publisher = publisher
	extension.Version = version
	extension.ReferenceName = referenceName
	extension.State = state

	if len(privateConfigurationValue) > 0 {
		privateConfig := ResourceExtensionParameter{}
		privateConfig.Key = "ignored"
		privateConfig.Value = base64.StdEncoding.EncodeToString([]byte(privateConfigurationValue))
		privateConfig.Type = "Private"

		extension.ResourceExtensionParameterValues.ResourceExtensionParameterValue = append(extension.ResourceExtensionParameterValues.ResourceExtensionParameterValue, privateConfig)
	}

	if len(publicConfigurationValue) > 0 {
		publicConfig := ResourceExtensionParameter{}
		publicConfig.Key = "ignored"
		publicConfig.Value = base64.StdEncoding.EncodeToString([]byte(publicConfigurationValue))
		publicConfig.Type = "Public"

		extension.ResourceExtensionParameterValues.ResourceExtensionParameterValue = append(extension.ResourceExtensionParameterValues.ResourceExtensionParameterValue, publicConfig)
	}

	azureVMConfiguration.ResourceExtensionReferences.ResourceExtensionReference = append(azureVMConfiguration.ResourceExtensionReferences.ResourceExtensionReference, extension)

	return azureVMConfiguration
}

func SetAzureDockerVMExtension(azureVMConfiguration *Role, dockerCertDir string, dockerPort int, version string) (*Role, error) {
	if len(version) == 0 {
		version = "0.3"
	}

	err := addDockerPort(azureVMConfiguration.ConfigurationSets.ConfigurationSet, dockerPort)
	if err != nil {
		return nil, err
	}

	publicConfiguration := createDockerPublicConfig(dockerPort)
	privateConfiguration, err := createDockerPrivateConfig(dockerCertDir)
	if err != nil {
		return nil, err
	}

	azureVMConfiguration = SetAzureVMExtension(azureVMConfiguration, "DockerExtension", "MSOpenTech.Extensions", version, "DockerExtension", "enable", publicConfiguration, privateConfiguration)
	return azureVMConfiguration, nil
}

func GetVMDeployment(cloudserviceName, deploymentName string) (*VMDeployment, error) {
	deployment := new(VMDeployment)

	requestURL := fmt.Sprintf(azureDeploymentURL, cloudserviceName, deploymentName)
	response, azureErr := azure.SendAzureGetRequest(requestURL)
	if azureErr != nil {
		return nil, azureErr
	}

	err := xml.Unmarshal(response, deployment)
	if err != nil {
		return nil, err
	}

	return deployment, nil
}

func DeleteVMDeployment(cloudserviceName, deploymentName string) error {

	requestURL :=  fmt.Sprintf(azureDeploymentURL, cloudserviceName, deploymentName)
	requestId, err := azure.SendAzureDeleteRequest(requestURL)
	if err != nil {
		return err
	}

	azure.WaitAsyncOperation(requestId)
	return nil
}

func GetRole(cloudserviceName, deploymentName, roleName string) (*Role, error) {
	role := new(Role)

	requestURL :=  fmt.Sprintf(azureRoleURL, cloudserviceName, deploymentName, roleName)
	response, azureErr := azure.SendAzureGetRequest(requestURL)
	if azureErr != nil {
		return nil, azureErr
	}

	err := xml.Unmarshal(response, role)
	if err != nil {
		return nil, err
	}

	return role, nil
}

func StartRole(cloudserviceName, deploymentName, roleName string) (error) {
	startRoleOperation := createStartRoleOperation()

	startRoleOperationBytes, err := xml.Marshal(startRoleOperation)
	if err != nil {
		return err
	}

	requestURL := fmt.Sprintf(azureOperationsURL, cloudserviceName, deploymentName, roleName)
	requestId, azureErr := azure.SendAzurePostRequest(requestURL, startRoleOperationBytes)
	if azureErr != nil {
		return azureErr
	}

	azure.WaitAsyncOperation(requestId)
	return nil
}

func ShutdownRole(cloudserviceName, deploymentName, roleName string) (error) {
	shutdownRoleOperation := createShutdowRoleOperation()

	shutdownRoleOperationBytes, err := xml.Marshal(shutdownRoleOperation)
	if err != nil {
		return err
	}

	requestURL := fmt.Sprintf(azureOperationsURL, cloudserviceName, deploymentName, roleName)
	requestId, azureErr := azure.SendAzurePostRequest(requestURL, shutdownRoleOperationBytes)
	if azureErr != nil {
		return azureErr
	}

	azure.WaitAsyncOperation(requestId)
	return nil
}

func RestartRole(cloudserviceName, deploymentName, roleName string) (error) {
	restartRoleOperation := createRestartRoleOperation()

	restartRoleOperationBytes, err := xml.Marshal(restartRoleOperation)
	if err != nil {
		return err
	}

	requestURL :=  fmt.Sprintf(azureOperationsURL, cloudserviceName, deploymentName, roleName)
	requestId, azureErr := azure.SendAzurePostRequest(requestURL, restartRoleOperationBytes)
	if azureErr != nil {
		return azureErr
	}

	azure.WaitAsyncOperation(requestId)
	return nil
}

func DeleteRole(cloudserviceName, deploymentName, roleName string) (error) {
	requestURL :=  fmt.Sprintf(azureRoleURL, cloudserviceName, deploymentName, roleName)
	requestId, azureErr := azure.SendAzureDeleteRequest(requestURL)
	if azureErr != nil {
		return azureErr
	}

	azure.WaitAsyncOperation(requestId)
	return nil
}

// REGION PUBLIC METHODS ENDS


// REGION PRIVATE METHODS STARTS

func createStartRoleOperation() StartRoleOperation {
	startRoleOperation := StartRoleOperation{}
	startRoleOperation.OperationType = "StartRoleOperation"
	startRoleOperation.Xmlns = azureXmlns

	return startRoleOperation
}

func createShutdowRoleOperation() ShutdownRoleOperation {
	shutdownRoleOperation := ShutdownRoleOperation{}
	shutdownRoleOperation.OperationType = "ShutdownRoleOperation"
	shutdownRoleOperation.Xmlns = azureXmlns

	return shutdownRoleOperation
}

func createRestartRoleOperation() RestartRoleOperation {
	startRoleOperation := RestartRoleOperation{}
	startRoleOperation.OperationType = "RestartRoleOperation"
	startRoleOperation.Xmlns = azureXmlns

	return startRoleOperation
}

func createDockerPublicConfig(dockerPort int) string{
	config := fmt.Sprintf(dockerPublicConfig, dockerPort)
	return config
}

func createDockerPrivateConfig(dockerCertDir string) (string, error) {
	usr, err := user.Current()
	if err != nil {
		return "", err
	}

	certDir := path.Join(usr.HomeDir, dockerCertDir)

	if _, err := os.Stat(certDir); err == nil {
		fmt.Println(dockerDirExistsMessage)
	} else {
		return "", errors.New(missingDockerCertsError)
	}

	caCert, err := parseFileToBase64String(path.Join(certDir, "ca.pem"))
	if err != nil {
		return "", err
	}

	serverCert, err := parseFileToBase64String(path.Join(certDir, "server-cert.pem"))
	if err != nil {
		return "", err
	}

	serverKey, err := parseFileToBase64String(path.Join(certDir, "server-key.pem"))
	if err != nil {
		return "", err
	}

	config := fmt.Sprintf(dockerPrivateConfig, caCert, serverCert, serverKey)
	return config, nil
}

func parseFileToBase64String(path string) (string, error) {
	caCertBytes, caErr := ioutil.ReadFile(path)
	if caErr != nil {
		return "", caErr
	}

	base64Content := base64.StdEncoding.EncodeToString(caCertBytes)
	return base64Content, nil
}

func addDockerPort(configurationSets []ConfigurationSet,  dockerPort int) error {
	if len(configurationSets) == 0 {
		return errors.New(provisioningConfDoesNotExistsError)
	}

	for i := 0; i < len(configurationSets); i++ {
		if configurationSets[i].ConfigurationSetType != "NetworkConfiguration" {
			continue
		}

		dockerEndpoint := createEndpoint("docker", "tcp", dockerPort, dockerPort)
		configurationSets[i].InputEndpoints.InputEndpoint = append(configurationSets[i].InputEndpoints.InputEndpoint, dockerEndpoint)
	}

	return nil
}

func createHostedServiceDeploymentConfig(dnsName, location string) (HostedServiceDeployment) {
	deployment := HostedServiceDeployment{}
	deployment.ServiceName = dnsName
	label := base64.StdEncoding.EncodeToString([]byte(dnsName))
	deployment.Label = label
	deployment.Location = location
	deployment.Xmlns = azureXmlns

	return deployment
}

func createVMDeploymentConfig(role *Role) (VMDeployment) {
	deployment := VMDeployment{}
	deployment.Name = role.RoleName
	deployment.Xmlns = azureXmlns
	deployment.DeploymentSlot = "Production"
	deployment.Label = role.RoleName
	deployment.RoleList.Role = append(deployment.RoleList.Role, role)

	return deployment
}

func createAzureVMRole(name, instanceSize, imageName, location string) (*Role, error){
	config := new(Role)
	config.RoleName = name
	config.RoleSize = instanceSize
	config.RoleType = "PersistentVMRole"
	config.ProvisionGuestAgent = true
	var err error
	config.OSVirtualHardDisk, err = createOSVirtualHardDisk(name, imageName, location)
	if err != nil {
		return nil, err
	}

	return config, nil
}

func createOSVirtualHardDisk(dnsName, imageName, location string) (OSVirtualHardDisk, error){
	oSVirtualHardDisk := OSVirtualHardDisk{}

	err := imageClient.ResolveImageName(imageName)
	if err != nil {
		return oSVirtualHardDisk, err
	}

	oSVirtualHardDisk.SourceImageName = imageName
	oSVirtualHardDisk.MediaLink, err = getVHDMediaLink(dnsName, location)
	if err != nil {
		return oSVirtualHardDisk, err
	}

	return oSVirtualHardDisk, nil
}

func getVHDMediaLink(dnsName, location string) (string, error){

	storageService, err := storageServiceClient.GetStorageServiceByLocation(location)
	if err != nil {
		return "", err
	}

	if storageService == nil {

		uuid, err := newUUID()
		if err != nil {
			return "", err
		}

		serviceName := "portalvhds" + uuid
		storageService, err = storageServiceClient.CreateStorageService(serviceName, location)
		if err != nil {
			return "", err
		}
	}

	blobEndpoint, err := storageServiceClient.GetBlobEndpoint(storageService)
	if err != nil {
		return "", err
	}

	vhdMediaLink := blobEndpoint + "vhds/" + dnsName + "-" + time.Now().Local().Format("20060102150405") + ".vhd"
	return vhdMediaLink, nil
}

// newUUID generates a random UUID according to RFC 4122
func newUUID() (string, error) {
	uuid := make([]byte, 16)
	n, err := io.ReadFull(rand.Reader, uuid)
	if n != len(uuid) || err != nil {
		return "", err
	}
	// variant bits; see section 4.1.1
	uuid[8] = uuid[8]&^0xc0 | 0x80
	// version 4 (pseudo-random); see section 4.1.3
	uuid[6] = uuid[6]&^0xf0 | 0x40

	//return fmt.Sprintf("%x-%x-%x-%x-%x", uuid[0:4], uuid[4:6], uuid[6:8], uuid[8:10], uuid[10:]), nil
	return fmt.Sprintf("%x", uuid[10:]), nil
}

func createLinuxProvisioningConfig(dnsName, userName, userPassword, certPath string) (ConfigurationSet, error) {
	provisioningConfig := ConfigurationSet{}

	disableSshPasswordAuthentication := false
	if len(userPassword) == 0 {
		disableSshPasswordAuthentication = true
		// We need to set dummy password otherwise azure API will throw an error
		userPassword = "P@ssword1"
	}

	provisioningConfig.DisableSshPasswordAuthentication = disableSshPasswordAuthentication
	provisioningConfig.ConfigurationSetType = "LinuxProvisioningConfiguration"
	provisioningConfig.HostName = dnsName
	provisioningConfig.UserName = userName
	provisioningConfig.UserPassword = userPassword

	if len(certPath) > 0 {
		var err error
		provisioningConfig.SSH, err = createSshConfig(certPath, userName)
		if err != nil {
			return provisioningConfig, err
		}
	}

	return provisioningConfig, nil
}

func uploadServiceCert(dnsName, certPath string) (error) {
	certificateConfig, err := createServiceCertDeploymentConf(certPath)
	if err != nil {
		return err
	}

	certificateConfigBytes, err := xml.Marshal(certificateConfig)
	if err != nil {
		return err
	}

	requestURL :=  fmt.Sprintf(azureCertificatListURL, dnsName)
	requestId, azureErr := azure.SendAzurePostRequest(requestURL, certificateConfigBytes)
	if azureErr != nil {
		return azureErr
	}

	err = azure.WaitAsyncOperation(requestId)
	return err
}

func createServiceCertDeploymentConf(certPath string) (ServiceCertificate, error) {
	certConfig := ServiceCertificate{}
	certConfig.Xmlns = azureXmlns
	data , err := ioutil.ReadFile(certPath)
	if err != nil {
		return certConfig, err
	}

	certData := base64.StdEncoding.EncodeToString(data)
	certConfig.Data = certData
	certConfig.CertificateFormat = "pfx"

	return certConfig, nil
}

func createSshConfig(certPath, userName string) (SSH, error) {
	sshConfig := SSH{}
	publicKey := PublicKey{}

	err := checkServiceCertExtension(certPath)
	if err != nil {
		return sshConfig, err
	}

	fingerprint, err := getServiceCertFingerprint(certPath)
	if err != nil {
		return sshConfig, err
	}

	publicKey.Fingerprint = fingerprint
	publicKey.Path = "/home/" + userName + "/.ssh/authorized_keys"

	sshConfig.PublicKeys.PublicKey = append(sshConfig.PublicKeys.PublicKey, publicKey)
	return sshConfig, nil
}

func getServiceCertFingerprint(certPath string) (string, error) {
	certData, readErr := ioutil.ReadFile(certPath)
	if readErr != nil {
		return "", readErr
	}
	
	block, rest := pem.Decode(certData)
	if block == nil {
		return "", errors.New(string(rest))
	}

	sha1sum := sha1.Sum(block.Bytes)
	fingerprint := fmt.Sprintf("%X", sha1sum)
	return fingerprint, nil
}

func checkServiceCertExtension(certPath string) (error) {
	certParts := strings.Split(certPath, ".")
	certExt := certParts[len(certParts) - 1]

	acceptedExtension := "pem"
	if certExt != acceptedExtension {
		return errors.New(fmt.Sprintf(invalidCertExtensionError, certPath, acceptedExtension))
	}

	return nil
}

func createNetworkConfig(os string) (ConfigurationSet, error) {
	networkConfig := ConfigurationSet{}
	networkConfig.ConfigurationSetType = "NetworkConfiguration"

	var endpoint InputEndpoint
	if os == osLinux {
		endpoint = createEndpoint("ssh", "tcp", 22, 22)
	} else if os == osWindows {
		//!TODO add rdp endpoint
	} else {
		return networkConfig, errors.New(fmt.Sprintf(invalidOSError))
	}

	networkConfig.InputEndpoints.InputEndpoint = append(networkConfig.InputEndpoints.InputEndpoint, endpoint)

	return networkConfig, nil
}

func createEndpoint(name string, protocol string, extertalPort int, internalPort int) (InputEndpoint) {
	endpoint := InputEndpoint{}
	endpoint.Name = name
	endpoint.Protocol = protocol
	endpoint.Port = extertalPort
	endpoint.LocalPort = internalPort

	return endpoint
}

// REGION PRIVATE METHODS ENDS


