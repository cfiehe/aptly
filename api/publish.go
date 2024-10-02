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
	Skip           bool
	GpgKey         string
	Keyring        string
	SecretKeyring  string
	Passphrase     string
	PassphraseFile string
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

// @Summary Get published repositories
// @Description Get a list of published repositories. Each published repository is returned as in "show" API.
// @Tags Publish
// @Produce json
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

// @Summary Show published repository
// @Description Get published repository by name.
// @Tags Publish
// @Consume json
// @Produce json
// @Param prefix path string true "publishing prefix, use ':.' instead of '.' because it is ambigious in URLs"
// @Param distribution path string true "distribution name"
// @Success 200 {object} deb.RemoteRepo
// @Failure 404 {object} Error "Published repository not found"
// @Failure 500 {object} Error "Internal Error"
// @Router /api/publish/{prefix}/{distribution} [get]
func apiPublishShow(c *gin.Context) {
	param := parseEscapedPath(c.Params.ByName("prefix"))
	storage, prefix := deb.ParsePrefix(param)
	distribution := parseEscapedPath(c.Params.ByName("distribution"))

	collectionFactory := context.NewCollectionFactory()
	collection := collectionFactory.PublishedRepoCollection()

	published, err := collection.ByStoragePrefixDistribution(storage, prefix, distribution)
	if err != nil {
		AbortWithJSONError(c, http.StatusNotFound, fmt.Errorf("unable to show: %s", err))
		return
	}
	err = collection.LoadComplete(published, collectionFactory)
	if err != nil {
		AbortWithJSONError(c, 500, fmt.Errorf("unable to show: %s", err))
		return
	}

	c.JSON(200, published)
}

// @Summary Create published repository
// @Description Create a published repository with specified parameters.
// @Tags Publish
// @Accept json
// @Produce json
// @Param prefix path string true "publishing prefix"
// @Success 200 {object} deb.RemoteRepo
// @Failure 400 {object} Error "Bad Request"
// @Failure 404 {object} Error "Source not found"
// @Failure 500 {object} Error "Internal Error"
// @Router /api/publish/{prefix} [post]
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
		MultiDist            *bool
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
		AbortWithJSONError(c, http.StatusBadRequest, fmt.Errorf("unable to publish: soures are empty"))
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
				AbortWithJSONError(c, http.StatusNotFound, fmt.Errorf("unable to publish: %s", err))
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
				AbortWithJSONError(c, http.StatusNotFound, fmt.Errorf("unable to publish: %s", err))
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
		AbortWithJSONError(c, http.StatusBadRequest, fmt.Errorf("unknown SourceKind"))
		return
	}

	published, err := deb.NewPublishedRepo(storage, prefix, b.Distribution, b.Architectures, components, sources, collectionFactory)
	if err != nil {
		AbortWithJSONError(c, 500, fmt.Errorf("unable to publish: %s", err))
		return
	}

	resources = append(resources, string(published.Key()))
	collection := collectionFactory.PublishedRepoCollection()

	taskName := fmt.Sprintf("Publish %s repository %s/%s with components \"%s\" and sources \"%s\"",
		b.SourceKind, published.StoragePrefix(), published.Distribution, strings.Join(components, `", "`), strings.Join(names, `", "`))
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

		if b.MultiDist != nil {
			published.MultiDist = *b.MultiDist
		}

		duplicate := collection.CheckDuplicate(published)
		if duplicate != nil {
			collectionFactory.PublishedRepoCollection().LoadComplete(duplicate, collectionFactory)
			return &task.ProcessReturnValue{Code: http.StatusBadRequest, Value: nil}, fmt.Errorf("prefix/distribution already used by another published repo: %s", duplicate)
		}

		err := published.Publish(context.PackagePool(), context, collectionFactory, signer, publishOutput, b.ForceOverwrite)
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

// @Summary Update published repository
// @Description Update a published repository.
// @Tags Publish
// @Accept json
// @Produce json
// @Param prefix path string true "publishing prefix"
// @Param distribution path string true "distribution name"
// @Success 200 {object} deb.RemoteRepo
// @Failure 400 {object} Error "Bad Request"
// @Failure 404 {object} Error "Published repository or source not found"
// @Failure 500 {object} Error "Internal Error"
// @Router /api/publish/{prefix}/{distribution} [put]
func apiPublishUpdateSwitch(c *gin.Context) {
	param := parseEscapedPath(c.Params.ByName("prefix"))
	storage, prefix := deb.ParsePrefix(param)
	distribution := parseEscapedPath(c.Params.ByName("distribution"))

	var b struct {
		AcquireByHash  *bool
		ForceOverwrite bool
		MultiDist      *bool
		Signing        SigningOptions
		SkipBz2        *bool
		SkipCleanup    *bool
		SkipContents   *bool
		Snapshots      []struct {
			Component string `binding:"required"`
			Name      string `binding:"required"`
		}
		Sources []struct {
			Component string `binding:"required"`
			Name      string `binding:"required"`
		}
	}

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

	var (
		sources           map[string]string
		updatedComponents []string
		updatedSources    []string
		removedComponents []string
	)

	if b.Sources != nil {
		sources = make(map[string]string, len(b.Sources))
		// For faster access, turn list of sources into map (component -> name)
		for _, source := range b.Sources {
			sources[source.Component] = source.Name
		}

		// Determine those components, which must be removed from the published repository
		for _, component := range published.Components() {
			_, exists := sources[component]
			if !exists {
				published.RemoveComponent(component)
				removedComponents = append(removedComponents, component)
			}
		}
	}

	if published.SourceKind == deb.SourceLocalRepo {
		if len(b.Snapshots) > 0 {
			AbortWithJSONError(c, http.StatusNotFound, fmt.Errorf("snapshots shouldn't be given when updating local repo"))
			return
		}
		localRepoCollection := collectionFactory.LocalRepoCollection()
		if sources != nil {
			for component, name := range sources {
				localRepo, err := localRepoCollection.ByName(name)
				if err != nil {
					AbortWithJSONError(c, http.StatusNotFound, err)
					return
				}

				err = localRepoCollection.LoadComplete(localRepo)
				if err != nil {
					AbortWithJSONError(c, http.StatusInternalServerError, err)
					return
				}

				sourceUUID, exists := published.Sources[component]
				if exists {
					if localRepo.UUID != sourceUUID {
						published.UpsertLocalRepo(component, localRepo)
						updatedComponents = append(updatedComponents, component)
						updatedSources = append(updatedSources, name)
					}
				} else {
					published.UpsertLocalRepo(component, localRepo)
					updatedComponents = append(updatedComponents, component)
					updatedSources = append(updatedSources, name)
				}
			}
		} else {
			// Republish (update) packages of published local repository
			updatedComponents = published.Components()
			for _, component := range updatedComponents {
				published.UpdateLocalRepo(component)
			}
		}
	} else if published.SourceKind == deb.SourceSnapshot {
		// For reasons of backward compatibility, resort to the former 'Snapshots' attribute
		// if the newer 'Sources' attribute is not specified.
		if b.Sources == nil && b.Snapshots != nil {
			fmt.Printf("@rDEPRECATION WARNING@|: The usage of the 'Snapshots' attribute for switching snapshots of a published repository is deprecated. " +
				"Use the 'Sources' attribute instead and specify the entire sources list. " +
				"Sources, which are missing in the list, but exist in the published repository, are removed. " +
				"Support of the 'Snapshots' attribute is dropped in one of the subsequent releases.")

			sources = make(map[string]string, len(b.Snapshots))
			for _, source := range b.Snapshots {
				sources[source.Component] = source.Name
			}
		}
		snapshotCollection := collectionFactory.SnapshotCollection()
		for component, name := range sources {
			snapshot, err := snapshotCollection.ByName(name)
			if err != nil {
				AbortWithJSONError(c, http.StatusNotFound, err)
				return
			}

			err = snapshotCollection.LoadComplete(snapshot)
			if err != nil {
				AbortWithJSONError(c, http.StatusInternalServerError, err)
				return
			}

			sourceUUID, exists := published.Sources[component]
			if exists {
				if snapshot.UUID != sourceUUID {
					published.UpsertSnapshot(component, snapshot)
					updatedComponents = append(updatedComponents, component)
					updatedSources = append(updatedSources, name)
				}
			} else {
				published.UpsertSnapshot(component, snapshot)
				updatedComponents = append(updatedComponents, component)
				updatedSources = append(updatedSources, name)
			}
		}
	} else {
		AbortWithJSONError(c, http.StatusInternalServerError, fmt.Errorf("unknown published repository type"))
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

	if b.MultiDist != nil {
		published.MultiDist = *b.MultiDist
	}

	resources := []string{string(published.Key())}

	taskName := fmt.Sprintf("Update published %s repository %s/%s with components \"%s\" and sources \"%s\"",
		published.SourceKind, published.StoragePrefix(), published.Distribution, strings.Join(updatedComponents, `", "`),
		strings.Join(updatedSources, `", "`))
	maybeRunTaskInBackground(c, taskName, resources, func(out aptly.Progress, _ *task.Detail) (*task.ProcessReturnValue, error) {
		err := published.Publish(context.PackagePool(), context, collectionFactory, signer, out, b.ForceOverwrite)
		if err != nil {
			return &task.ProcessReturnValue{Code: http.StatusInternalServerError, Value: nil}, fmt.Errorf("unable to update: %s", err)
		}

		err = collection.Update(published)
		if err != nil {
			return &task.ProcessReturnValue{Code: http.StatusInternalServerError, Value: nil}, fmt.Errorf("unable to save to DB: %s", err)
		}

		if b.SkipCleanup == nil || !*b.SkipCleanup {
			publishedStorage := context.GetPublishedStorage(storage)

			err = collection.CleanupPrefixComponentFiles(published.Prefix, updatedComponents, publishedStorage, collectionFactory, out)
			if err != nil {
				return &task.ProcessReturnValue{Code: http.StatusInternalServerError, Value: nil}, fmt.Errorf("unable to update: %s", err)
			}

			if len(removedComponents) > 0 {
				// Cleanup files belonging to a removed component by dropping the component directory from the storage backend.
				for _, component := range removedComponents {
					err = publishedStorage.RemoveDirs(filepath.Join(prefix, "dists", distribution, component), out)
					if err != nil {
						return &task.ProcessReturnValue{Code: http.StatusInternalServerError, Value: nil}, fmt.Errorf("unable to update: %s", err)
					}
				}
			}
		}

		return &task.ProcessReturnValue{Code: http.StatusOK, Value: published}, nil
	})
}

// @Summary Delete published repository
// @Description Delete a published repository.
// @Tags Publish
// @Accept json
// @Produce json
// @Param prefix path string true "publishing prefix"
// @Param distribution path string true "distribution name"
// @Param force query int true "force: 1 to enable"
// @Param skipCleanup query int true "skipCleanup: 1 to enable"
// @Success 200 {object} task.ProcessReturnValue
// @Failure 400 {object} Error "Bad Request"
// @Failure 404 {object} Error "Published repository not found"
// @Failure 500 {object} Error "Internal Error"
// @Router /api/publish/{prefix}/{distribution} [delete]
func apiPublishDrop(c *gin.Context) {
	force := c.Request.URL.Query().Get("force") == "1"
	skipCleanup := c.Request.URL.Query().Get("SkipCleanup") == "1"

	param := parseEscapedPath(c.Params.ByName("prefix"))
	storage, prefix := deb.ParsePrefix(param)
	distribution := parseEscapedPath(c.Params.ByName("distribution"))

	collectionFactory := context.NewCollectionFactory()
	collection := collectionFactory.PublishedRepoCollection()

	published, err := collection.ByStoragePrefixDistribution(storage, prefix, distribution)
	if err != nil {
		AbortWithJSONError(c, http.StatusNotFound, fmt.Errorf("unable to drop: %s", err))
		return
	}

	resources := []string{string(published.Key())}

	taskName := fmt.Sprintf("Delete published %s repository %s/%s", published.SourceKind, published.StoragePrefix(), published.Distribution)
	maybeRunTaskInBackground(c, taskName, resources, func(out aptly.Progress, _ *task.Detail) (*task.ProcessReturnValue, error) {
		err := collection.Remove(context, storage, prefix, distribution,
			collectionFactory, out, force, skipCleanup)
		if err != nil {
			return &task.ProcessReturnValue{Code: http.StatusInternalServerError, Value: nil}, fmt.Errorf("unable to drop: %s", err)
		}

		return &task.ProcessReturnValue{Code: http.StatusOK, Value: gin.H{}}, nil
	})
}
