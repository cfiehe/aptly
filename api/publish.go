package api

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/aptly-dev/aptly/aptly"
	"github.com/aptly-dev/aptly/deb"
	"github.com/aptly-dev/aptly/pgp"
	"github.com/aptly-dev/aptly/task"
	"github.com/gin-gonic/gin"
)

// SigningOptions is a shared between publish API GPG options structure
type SigningOptions struct {
	// Set "true" to skip signing published repository
	Skip bool `             json:"Skip"           example:"false"`
	// key name (local to aptly service user)
	GpgKey string `         json:"GpgKey"         example:"reposign@aptly.info"`
	// keyring filename (local to aptly service user)
	Keyring string `        json:"Keyring"        example:"trustedkeys.gpg"`
	// OBSOLETE (gpg1): secret keyring filename (local to aptly server user)
	SecretKeyring string `  json:"SecretKeyring"  example:"aptly.sec"`
	// key passphrase (if using over http, would be transmitted in clear text!)
	Passphrase string `     json:"Passphrase"`
	// passphrase file (local to aptly service user)
	PassphraseFile string ` json:"PassphraseFile" example:"/etc/aptly.pass"`
}

func getSigner(options *SigningOptions) (pgp.Signer, error) {
	if options.Skip {
		return nil, nil
	}

	signer := context.GetSigner()
	signer.SetKey(options.GpgKey)
	signer.SetKeyRing(options.Keyring, options.SecretKeyring)
	signer.SetPassphrase(options.Passphrase, options.PassphraseFile)

	// If Batch is false, GPG will ask for passphrase on stdin, which would block the api process
	signer.SetBatch(true)

	err := signer.Init()
	if err != nil {
		return nil, err
	}

	return signer, nil
}

// Replace '_' with '/' and double '__' with single '_'
func parseEscapedPath(path string) string {
	result := strings.Replace(strings.Replace(path, "_", "/", -1), "//", "_", -1)
	if result == "" {
		result = "."
	}
	return result
}

// @Summary Get publish points
// @Description Get list of available publish points. Each publish point is returned as in “show” API.
// @Tags Publish
// @Produce  json
// @Success 200 {array} deb.PublishedRepo
// @Failure 500 {object} Error "Internal Error"
// @Router /api/publish [get]
func apiPublishList(c *gin.Context) {
	collectionFactory := context.NewCollectionFactory()
	collection := collectionFactory.PublishedRepoCollection()

	result := make([]*deb.PublishedRepo, 0, collection.Len())

	err := collection.ForEach(func(repo *deb.PublishedRepo) error {
		err := collection.LoadShallow(repo, collectionFactory)
		if err != nil {
			return err
		}

		result = append(result, repo)

		return nil
	})

	if err != nil {
		AbortWithJSONError(c, 500, err)
		return
	}

	c.JSON(200, result)
}

// POST /publish/:prefix
func apiPublishRepoOrSnapshot(c *gin.Context) {
	param := parseEscapedPath(c.Params.ByName("prefix"))
	storage, prefix := deb.ParsePrefix(param)

	var b struct {
		SourceKind string `binding:"required"`
		Sources    []struct {
			Component string
			Name      string `binding:"required"`
		} `binding:"required"`
		Distribution         string
		Label                string
		Origin               string
		NotAutomatic         string
		ButAutomaticUpgrades string
		ForceOverwrite       bool
		SkipContents         *bool
		SkipBz2              *bool
		Architectures        []string
		Signing              SigningOptions
		AcquireByHash        *bool
		MultiDist            bool
	}

	if c.Bind(&b) != nil {
		return
	}

	signer, err := getSigner(&b.Signing)
	if err != nil {
		AbortWithJSONError(c, 500, fmt.Errorf("unable to initialize GPG signer: %s", err))
		return
	}

	if len(b.Sources) == 0 {
		AbortWithJSONError(c, 400, fmt.Errorf("unable to publish: soures are empty"))
		return
	}

	var components []string
	var names []string
	var sources []interface{}
	var resources []string
	collectionFactory := context.NewCollectionFactory()

	if b.SourceKind == "snapshot" {
		var snapshot *deb.Snapshot

		snapshotCollection := collectionFactory.SnapshotCollection()

		for _, source := range b.Sources {
			components = append(components, source.Component)
			names = append(names, source.Name)

			snapshot, err = snapshotCollection.ByName(source.Name)
			if err != nil {
				AbortWithJSONError(c, 404, fmt.Errorf("unable to publish: %s", err))
				return
			}

			resources = append(resources, string(snapshot.ResourceKey()))
			err = snapshotCollection.LoadComplete(snapshot)
			if err != nil {
				AbortWithJSONError(c, 500, fmt.Errorf("unable to publish: %s", err))
				return
			}

			sources = append(sources, snapshot)
		}
	} else if b.SourceKind == deb.SourceLocalRepo {
		var localRepo *deb.LocalRepo

		localCollection := collectionFactory.LocalRepoCollection()

		for _, source := range b.Sources {
			components = append(components, source.Component)
			names = append(names, source.Name)

			localRepo, err = localCollection.ByName(source.Name)
			if err != nil {
				AbortWithJSONError(c, 404, fmt.Errorf("unable to publish: %s", err))
				return
			}

			resources = append(resources, string(localRepo.Key()))
			err = localCollection.LoadComplete(localRepo)
			if err != nil {
				AbortWithJSONError(c, 500, fmt.Errorf("unable to publish: %s", err))
			}

			sources = append(sources, localRepo)
		}
	} else {
		AbortWithJSONError(c, 400, fmt.Errorf("unknown SourceKind"))
		return
	}

	published, err := deb.NewPublishedRepo(storage, prefix, b.Distribution, b.Architectures, components, sources, collectionFactory)
	if err != nil {
		AbortWithJSONError(c, 500, fmt.Errorf("unable to publish: %s", err))
		return
	}

	resources = append(resources, string(published.Key()))
	collection := collectionFactory.PublishedRepoCollection()

	taskName := fmt.Sprintf("Publish %s: %s", b.SourceKind, strings.Join(names, ", "))
	maybeRunTaskInBackground(c, taskName, resources, func(out aptly.Progress, detail *task.Detail) (*task.ProcessReturnValue, error) {
		taskDetail := task.PublishDetail{
			Detail: detail,
		}
		publishOutput := &task.PublishOutput{
			Progress:      out,
			PublishDetail: taskDetail,
		}

		if b.Origin != "" {
			published.Origin = b.Origin
		}
		if b.NotAutomatic != "" {
			published.NotAutomatic = b.NotAutomatic
		}
		if b.ButAutomaticUpgrades != "" {
			published.ButAutomaticUpgrades = b.ButAutomaticUpgrades
		}
		published.Label = b.Label

		published.SkipContents = context.Config().SkipContentsPublishing
		if b.SkipContents != nil {
			published.SkipContents = *b.SkipContents
		}

		published.SkipBz2 = context.Config().SkipBz2Publishing
		if b.SkipBz2 != nil {
			published.SkipBz2 = *b.SkipBz2
		}

		if b.AcquireByHash != nil {
			published.AcquireByHash = *b.AcquireByHash
		}

		duplicate := collection.CheckDuplicate(published)
		if duplicate != nil {
			collectionFactory.PublishedRepoCollection().LoadComplete(duplicate, collectionFactory)
			return &task.ProcessReturnValue{Code: http.StatusBadRequest, Value: nil}, fmt.Errorf("prefix/distribution already used by another published repo: %s", duplicate)
		}

		err := published.Publish(context.PackagePool(), context, collectionFactory, signer, publishOutput, b.ForceOverwrite, b.MultiDist)
		if err != nil {
			return &task.ProcessReturnValue{Code: http.StatusInternalServerError, Value: nil}, fmt.Errorf("unable to publish: %s", err)
		}

		err = collection.Add(published)
		if err != nil {
			return &task.ProcessReturnValue{Code: http.StatusInternalServerError, Value: nil}, fmt.Errorf("unable to save to DB: %s", err)
		}

		return &task.ProcessReturnValue{Code: http.StatusCreated, Value: published}, nil
	})
}

// PUT /publish/:prefix/:distribution
func apiPublishUpdateSwitch(c *gin.Context) {
	param := parseEscapedPath(c.Params.ByName("prefix"))
	storage, prefix := deb.ParsePrefix(param)
	distribution := c.Params.ByName("distribution")

	var b struct {
		ForceOverwrite bool
		Signing        SigningOptions
		SkipContents   *bool
		SkipBz2        *bool
		SkipCleanup    *bool
		Snapshots      []struct {
			Component string `binding:"required"`
			Name      string `binding:"required"`
		}
		AcquireByHash *bool
		MultiDist     bool
	}

	if c.Bind(&b) != nil {
		return
	}

	signer, err := getSigner(&b.Signing)
	if err != nil {
		AbortWithJSONError(c, 500, fmt.Errorf("unable to initialize GPG signer: %s", err))
		return
	}

	collectionFactory := context.NewCollectionFactory()
	collection := collectionFactory.PublishedRepoCollection()

	published, err := collection.ByStoragePrefixDistribution(storage, prefix, distribution)
	if err != nil {
		AbortWithJSONError(c, 404, fmt.Errorf("unable to update: %s", err))
		return
	}
	err = collection.LoadComplete(published, collectionFactory)
	if err != nil {
		AbortWithJSONError(c, 500, fmt.Errorf("unable to update: %s", err))
		return
	}

	var updatedComponents []string
	var updatedSnapshots []string
	var resources []string

	if published.SourceKind == deb.SourceLocalRepo {
		if len(b.Snapshots) > 0 {
			AbortWithJSONError(c, 400, fmt.Errorf("snapshots shouldn't be given when updating local repo"))
			return
		}
		updatedComponents = published.Components()
		for _, component := range updatedComponents {
			published.UpdateLocalRepo(component)
		}
	} else if published.SourceKind == "snapshot" {
		for _, snapshotInfo := range b.Snapshots {
			snapshotCollection := collectionFactory.SnapshotCollection()
			snapshot, err2 := snapshotCollection.ByName(snapshotInfo.Name)
			if err2 != nil {
				AbortWithJSONError(c, 404, err2)
				return
			}

			err2 = snapshotCollection.LoadComplete(snapshot)
			if err2 != nil {
				AbortWithJSONError(c, 500, err2)
				return
			}

			published.UpdateSnapshot(snapshotInfo.Component, snapshot)
			updatedComponents = append(updatedComponents, snapshotInfo.Component)
			updatedSnapshots = append(updatedSnapshots, snapshot.Name)
		}
	} else {
		AbortWithJSONError(c, 500, fmt.Errorf("unknown published repository type"))
		return
	}

	if b.SkipContents != nil {
		published.SkipContents = *b.SkipContents
	}

	if b.SkipBz2 != nil {
		published.SkipBz2 = *b.SkipBz2
	}

	if b.AcquireByHash != nil {
		published.AcquireByHash = *b.AcquireByHash
	}

	resources = append(resources, string(published.Key()))
	taskName := fmt.Sprintf("Update published %s (%s): %s", published.SourceKind, strings.Join(updatedComponents, " "), strings.Join(updatedSnapshots, ", "))
	maybeRunTaskInBackground(c, taskName, resources, func(out aptly.Progress, _ *task.Detail) (*task.ProcessReturnValue, error) {
		err := published.Publish(context.PackagePool(), context, collectionFactory, signer, out, b.ForceOverwrite, b.MultiDist)
		if err != nil {
			return &task.ProcessReturnValue{Code: http.StatusInternalServerError, Value: nil}, fmt.Errorf("unable to update: %s", err)
		}

		err = collection.Update(published)
		if err != nil {
			return &task.ProcessReturnValue{Code: http.StatusInternalServerError, Value: nil}, fmt.Errorf("unable to save to DB: %s", err)
		}

		if b.SkipCleanup == nil || !*b.SkipCleanup {
			err = collection.CleanupPrefixComponentFiles(published.Prefix, updatedComponents,
				context.GetPublishedStorage(storage), collectionFactory, out)
			if err != nil {
				return &task.ProcessReturnValue{Code: http.StatusInternalServerError, Value: nil}, fmt.Errorf("unable to update: %s", err)
			}
		}

		return &task.ProcessReturnValue{Code: http.StatusOK, Value: published}, nil
	})
}

// DELETE /publish/:prefix/:distribution
func apiPublishDrop(c *gin.Context) {
	force := c.Request.URL.Query().Get("force") == "1"
	skipCleanup := c.Request.URL.Query().Get("SkipCleanup") == "1"

	param := parseEscapedPath(c.Params.ByName("prefix"))
	storage, prefix := deb.ParsePrefix(param)
	distribution := c.Params.ByName("distribution")

	collectionFactory := context.NewCollectionFactory()
	collection := collectionFactory.PublishedRepoCollection()

	published, err := collection.ByStoragePrefixDistribution(storage, prefix, distribution)
	if err != nil {
		AbortWithJSONError(c, http.StatusNotFound, fmt.Errorf("unable to drop: %s", err))
		return
	}

	resources := []string{string(published.Key())}

	taskName := fmt.Sprintf("Delete published %s (%s)", prefix, distribution)
	maybeRunTaskInBackground(c, taskName, resources, func(out aptly.Progress, _ *task.Detail) (*task.ProcessReturnValue, error) {
		err := collection.Remove(context, storage, prefix, distribution,
			collectionFactory, out, force, skipCleanup)
		if err != nil {
			return &task.ProcessReturnValue{Code: http.StatusInternalServerError, Value: nil}, fmt.Errorf("unable to drop: %s", err)
		}

		return &task.ProcessReturnValue{Code: http.StatusOK, Value: gin.H{}}, nil
	})
}

type publishRemoveParams struct {
	// Signing parameters
	Signing SigningOptions `                 json:"Signing"`
	// Do not remove unreferenced files in prefix/component
	SkipCleanup *bool `                      json:"SkipCleanup"     example:"false"`
	// List of Components to remove
	Components []string `binding:"required"  json:"Components"      example:"main"`
	// Enable multiple packages with the same filename in different distributions
	MultiDist bool `                         json:"MultiDist"       example:"false"`
	// Overwrite existing files in pool/ directory
	ForceOverwrite bool `                    json:"ForceOverwrite"  example:"false"`
}

// @Summary Remove sources
// @Description **Remove sources (snapshots / local repos) from publish**
// @Description
// @Description This removes specifies sources from a snapshot.
// @Tags Publish
// @Param prefix path string true "Publishing prefix, `.` for root"
// @Param distribution path string true "Distribution name"
// @Consume json
// @Param request body publishRemoveParams true "Parameters"
// @Produce json
// @Success 200 {object} task.ProcessReturnValue "Mirror was updated successfully"
// @Success 202 {object} task.Task "Mirror is being updated"
// @Failure 400 {object} Error "Unable to determine list of architectures"
// @Failure 404 {object} Error "Mirror not found"
// @Failure 500 {object} Error "Internal Error"
// @Router /api/publish/{prefix}/{distribution}/remove [post]
func apiPublishRemove(c *gin.Context) {
	param := parseEscapedPath(c.Params.ByName("prefix"))
	storage, prefix := deb.ParsePrefix(param)
	distribution := c.Params.ByName("distribution")

	var b publishRemoveParams
	if c.Bind(&b) != nil {
		return
	}

	signer, err := getSigner(&b.Signing)
	if err != nil {
		AbortWithJSONError(c, http.StatusInternalServerError, fmt.Errorf("unable to initialize GPG signer: %s", err))
		return
	}

	collectionFactory := context.NewCollectionFactory()
	collection := collectionFactory.PublishedRepoCollection()

	published, err := collection.ByStoragePrefixDistribution(storage, prefix, distribution)
	if err != nil {
		AbortWithJSONError(c, http.StatusNotFound, fmt.Errorf("unable to update: %s", err))
		return
	}
	err = collection.LoadComplete(published, collectionFactory)
	if err != nil {
		AbortWithJSONError(c, http.StatusInternalServerError, fmt.Errorf("unable to update: %s", err))
		return
	}

	components := b.Components

	for _, component := range components {
		_, exists := published.Sources[component]
		if !exists {
			AbortWithJSONError(c, http.StatusInternalServerError, fmt.Errorf("unable to update: '%s' is not a published component", component))
			return
		}
		published.DropComponent(component)
	}

	resources := []string{string(published.Key())}

	taskName := fmt.Sprintf("Remove components '%s' from publish %s (%s)", strings.Join(components, ","), prefix, distribution)
	maybeRunTaskInBackground(c, taskName, resources, func(out aptly.Progress, _ *task.Detail) (*task.ProcessReturnValue, error) {
		err := published.Publish(context.PackagePool(), context, collectionFactory, signer, out, b.ForceOverwrite, b.MultiDist)
		if err != nil {
			return &task.ProcessReturnValue{Code: http.StatusInternalServerError, Value: nil}, fmt.Errorf("unable to update: %s", err)
		}

		err = collection.Update(published)
		if err != nil {
			return &task.ProcessReturnValue{Code: http.StatusInternalServerError, Value: nil}, fmt.Errorf("unable to save to DB: %s", err)
		}

		if b.SkipCleanup == nil || !*b.SkipCleanup {
			publishedStorage := context.GetPublishedStorage(storage)

			err = collection.CleanupPrefixComponentFiles(published.Prefix, components, publishedStorage, collectionFactory, out)
			if err != nil {
				return &task.ProcessReturnValue{Code: http.StatusInternalServerError, Value: nil}, fmt.Errorf("unable to update: %s", err)
			}

			for _, component := range components {
				err = publishedStorage.RemoveDirs(filepath.Join(prefix, "dists", distribution, component), out)
				if err != nil {
					return &task.ProcessReturnValue{Code: http.StatusInternalServerError, Value: nil}, fmt.Errorf("unable to update: %s", err)
				}
			}
		}

		return &task.ProcessReturnValue{Code: http.StatusOK, Value: gin.H{}}, nil
	})
}
