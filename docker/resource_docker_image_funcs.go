package docker

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"

	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/docker/docker/pkg/term"
	"github.com/hashicorp/terraform-plugin-sdk/helper/schema"
)

func getBuildConfig(d *schema.ResourceData) map[string]interface{} {
	return d.Get("build").(*schema.Set).List()[0].(map[string]interface{})
}

func isBuilt(d *schema.ResourceData) bool {
	return len(d.Get("build").(*schema.Set).List()) > 0
}

func resourceDockerImageCreate(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*ProviderConfig).DockerClient
	imageName := d.Get("name").(string)
	if isBuilt(d) {
		buildConfig := getBuildConfig(d)
		contextTarHash, err := buildDockerImage(client, imageName, buildConfig)
		if err != nil {
			return fmt.Errorf("Unable to build Docker image : %s", err)
		}
		buildConfig["context_tar_hash"] = contextTarHash
	}
	apiImage, err := findImage(imageName, client, meta.(*ProviderConfig).AuthConfigs)
	if err != nil {
		return fmt.Errorf("Unable to read Docker image into resource: %s", err)
	}
	d.SetId(apiImage.ID + "/" + imageName)
	return resourceDockerImageRead(d, meta)
}

func resourceDockerImageRead(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*ProviderConfig).DockerClient
	var data Data
	if err := fetchLocalImages(&data, client); err != nil {
		return fmt.Errorf("Error reading docker image list: %s", err)
	}
	for id := range data.DockerImages {
		log.Printf("[DEBUG] local images data: %v", id)
	}
	foundImage := searchLocalImages(data, d.Get("name").(string))

	if foundImage == nil {
		d.SetId("")
		return nil
	}

	// if it's build then check the contexthash
	if isBuilt(d) {
		buildOptions := getBuildConfig(d)
		dockerContextTarPath, err := buildContextTar(buildOptions["context"].(string))
		if err != nil {
			return err
		}
		defer os.Remove(dockerContextTarPath)
		contextHash, err := getDockerContextTarHash(dockerContextTarPath)
		if err != nil {
			return err
		}
		if contextHash != buildOptions["context_tar_hash"] {
			d.Set("build", nil)
		}
	}

	d.SetId(foundImage.ID + d.Get("name").(string))
	d.Set("latest", foundImage.ID)
	return nil
}

func resourceDockerImageUpdate(d *schema.ResourceData, meta interface{}) error {
	// We need to re-read in case switching parameters affects
	// the value of "latest" or others
	client := meta.(*ProviderConfig).DockerClient
	imageName := d.Get("name").(string)
	apiImage, err := findImage(imageName, client, meta.(*ProviderConfig).AuthConfigs)
	if err != nil {
		return fmt.Errorf("Unable to read Docker image into resource: %s", err)
	}

	d.Set("latest", apiImage.ID)

	return resourceDockerImageRead(d, meta)
}

func resourceDockerImageDelete(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*ProviderConfig).DockerClient
	err := removeImage(d, client)
	if err != nil {
		return fmt.Errorf("Unable to remove Docker image: %s", err)
	}
	d.SetId("")
	return nil
}

func searchLocalImages(data Data, imageName string) *types.ImageSummary {
	if apiImage, ok := data.DockerImages[imageName]; ok {
		log.Printf("[DEBUG] found local image via imageName: %v", imageName)
		return apiImage
	}
	if apiImage, ok := data.DockerImages[imageName+":latest"]; ok {
		log.Printf("[DEBUG] found local image via imageName + latest: %v", imageName)
		imageName = imageName + ":latest"
		return apiImage
	}
	return nil
}

func removeImage(d *schema.ResourceData, client *client.Client) error {
	var data Data

	if keepLocally := d.Get("keep_locally").(bool); keepLocally {
		return nil
	}

	if err := fetchLocalImages(&data, client); err != nil {
		return err
	}

	imageName := d.Get("name").(string)
	if imageName == "" {
		return fmt.Errorf("Empty image name is not allowed")
	}

	foundImage := searchLocalImages(data, imageName)

	if foundImage != nil {
		imageDeleteResponseItems, err := client.ImageRemove(context.Background(), foundImage.ID, types.ImageRemoveOptions{})
		if err != nil {
			return err
		}
		log.Printf("[INFO] Deleted image items: %v", imageDeleteResponseItems)
	}

	return nil
}

func fetchLocalImages(data *Data, client *client.Client) error {
	images, err := client.ImageList(context.Background(), types.ImageListOptions{All: false})
	if err != nil {
		return fmt.Errorf("Unable to list Docker images: %s", err)
	}

	if data.DockerImages == nil {
		data.DockerImages = make(map[string]*types.ImageSummary)
	}

	// Docker uses different nomenclatures in different places...sometimes a short
	// ID, sometimes long, etc. So we store both in the map so we can always find
	// the same image object. We store the tags and digests, too.
	for i, image := range images {
		data.DockerImages[image.ID[:12]] = &images[i]
		data.DockerImages[image.ID] = &images[i]
		for _, repotag := range image.RepoTags {
			data.DockerImages[repotag] = &images[i]
		}
		for _, repodigest := range image.RepoDigests {
			data.DockerImages[repodigest] = &images[i]
		}
	}

	return nil
}

func pullImage(data *Data, client *client.Client, authConfig *AuthConfigs, image string) error {
	pullOpts := parseImageOptions(image)

	// If a registry was specified in the image name, try to find auth for it
	auth := types.AuthConfig{}
	if pullOpts.Registry != "" {
		if authConfig, ok := authConfig.Configs[normalizeRegistryAddress(pullOpts.Registry)]; ok {
			auth = authConfig
		}
	} else {
		// Try to find an auth config for the public docker hub if a registry wasn't given
		if authConfig, ok := authConfig.Configs["https://registry.hub.docker.com"]; ok {
			auth = authConfig
		}
	}

	encodedJSON, err := json.Marshal(auth)
	if err != nil {
		return fmt.Errorf("error creating auth config: %s", err)
	}

	out, err := client.ImagePull(context.Background(), image, types.ImagePullOptions{
		RegistryAuth: base64.URLEncoding.EncodeToString(encodedJSON),
	})
	if err != nil {
		return fmt.Errorf("error pulling image %s: %s", image, err)
	}
	defer out.Close()

	buf := new(bytes.Buffer)
	buf.ReadFrom(out)
	s := buf.String()
	log.Printf("[DEBUG] pulled image %v: %v", image, s)

	return nil
}

type internalPullImageOptions struct {
	Repository string `qs:"fromImage"`
	Tag        string

	// Only required for Docker Engine 1.9 or 1.10 w/ Remote API < 1.21
	// and Docker Engine < 1.9
	// This parameter was removed in Docker Engine 1.11
	Registry string
}

func parseImageOptions(image string) internalPullImageOptions {
	pullOpts := internalPullImageOptions{}

	// Pre-fill with image by default, update later if tag found
	pullOpts.Repository = image

	firstSlash := strings.Index(image, "/")

	// Detect the registry name - it should either contain port, be fully qualified or be localhost
	// If the image contains more than 2 path components, or at least one and the prefix looks like a hostname
	if strings.Count(image, "/") > 1 || firstSlash != -1 && (strings.ContainsAny(image[:firstSlash], ".:") || image[:firstSlash] == "localhost") {
		// registry/repo/image
		pullOpts.Registry = image[:firstSlash]
	}

	prefixLength := len(pullOpts.Registry)
	tagIndex := strings.Index(image[prefixLength:], ":")

	if tagIndex != -1 {
		// we have the tag, strip it
		pullOpts.Repository = image[:prefixLength+tagIndex]
		pullOpts.Tag = image[prefixLength+tagIndex+1:]
	}

	return pullOpts
}

func findImage(imageName string, client *client.Client, authConfig *AuthConfigs) (*types.ImageSummary, error) {
	log.Printf("[DEBUG] Finding Docker image: %s", imageName)
	if imageName == "" {
		return nil, fmt.Errorf("Empty image name is not allowed")
	}

	var data Data
	// load local images into the data structure
	if err := fetchLocalImages(&data, client); err != nil {
		return nil, err
	}

	foundImage := searchLocalImages(data, imageName)
	if foundImage != nil {
		return foundImage, nil
	}

	if err := pullImage(&data, client, authConfig, imageName); err != nil {
		return nil, fmt.Errorf("Unable to pull image %s: %s", imageName, err)
	}

	// update the data structure of the images
	if err := fetchLocalImages(&data, client); err != nil {
		return nil, err
	}

	foundImage = searchLocalImages(data, imageName)
	if foundImage != nil {
		return foundImage, nil
	}

	return nil, fmt.Errorf("Unable to find or pull image %s", imageName)
}

func buildContextTar(buildContext string) (string, error) {
	// Create our Temp File:  This will create a filename like /tmp/terraform-provider-docker-123456.tar
	tmpFile, err := ioutil.TempFile(os.TempDir(), "terraform-provider-docker-*.tar")
	if err != nil {
		return "", fmt.Errorf("Cannot create temporary file - %v", err.Error())
	}

	defer tmpFile.Close()

	if _, err = os.Stat(buildContext); err != nil {
		return "", fmt.Errorf("Unable to read build context - %v", err.Error())
	}

	tw := tar.NewWriter(tmpFile)
	defer tw.Close()

	err = filepath.Walk(buildContext, func(file string, info os.FileInfo, err error) error {

		// return on any error
		if err != nil {
			return err
		}

		// create a new dir/file header
		header, err := tar.FileInfoHeader(info, info.Name())
		if err != nil {
			return err
		}

		// update the name to correctly reflect the desired destination when untaring
		header.Name = strings.TrimPrefix(strings.Replace(file, buildContext, "", -1), string(filepath.Separator))

		// write the header
		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		// return on non-regular files (thanks to [kumo](https://medium.com/@komuw/just-like-you-did-fbdd7df829d3) for this suggested update)
		if !info.Mode().IsRegular() {
			return nil
		}

		// open files for taring
		f, err := os.Open(file)
		if err != nil {
			return err
		}

		// copy file data into tar writer
		if _, err := io.Copy(tw, f); err != nil {
			return err
		}

		// manually close here after each file operation; defering would cause each file close
		// to wait until all operations have completed.
		f.Close()

		return nil

	})

	return tmpFile.Name(), nil
}

func getDockerContextTarHash(dockerContextTarPath string) (string, error) {
	hasher := sha256.New()
	s, err := ioutil.ReadFile(dockerContextTarPath)
	if err != nil {
		return "", err
	}
	hasher.Write(s)
	contextHash := hex.EncodeToString(hasher.Sum(nil))
	return contextHash, nil
}

func buildDockerImage(client *client.Client, tag string, buildOptions map[string]interface{}) (string, error) {
	log.Printf("[DEBUG] Building docker image: %s", tag)
	dockerContextTarPath, err := buildContextTar(buildOptions["context"].(string))
	defer os.Remove(dockerContextTarPath)

	contextHash, err := getDockerContextTarHash(dockerContextTarPath)
	if err != nil {
		return "", err
	}

	dockerBuildContext, err := os.Open(dockerContextTarPath)
	defer dockerBuildContext.Close()

	options := types.ImageBuildOptions{
		SuppressOutput: false,
		Remove:         true,
		ForceRemove:    true,
		PullParent:     true,
		Tags:           []string{tag},
		Dockerfile:     buildOptions["dockerfile"].(string),
		BuildArgs:      mapTypeMapValsToStringPtr(buildOptions["buildargs"].(map[string]interface{})),
	}

	buildResponse, err := client.ImageBuild(context.Background(), dockerBuildContext, options)
	if err != nil {
		return "", err
	}
	defer buildResponse.Body.Close()

	termFd, isTerm := term.GetFdInfo(os.Stderr)
	err = jsonmessage.DisplayJSONMessagesStream(buildResponse.Body, os.Stderr, termFd, isTerm, nil)
	if err != nil {
		return "", err
	}

	return contextHash, nil
}

func mapTypeMapValsToStringPtr(typeMap map[string]interface{}) map[string]*string {
	mapped := make(map[string]*string, len(typeMap))
	for k, v := range typeMap {
		*mapped[k] = v.(string)
	}
	return mapped
}
