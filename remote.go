package git

import (
	"context"
	"errors"
	"fmt"
	"io"

	"gopkg.in/src-d/go-billy.v4/osfs"
	"gopkg.in/src-d/go-git.v4/config"
	"gopkg.in/src-d/go-git.v4/plumbing"
	"gopkg.in/src-d/go-git.v4/plumbing/cache"
	"gopkg.in/src-d/go-git.v4/plumbing/format/packfile"
	"gopkg.in/src-d/go-git.v4/plumbing/object"
	"gopkg.in/src-d/go-git.v4/plumbing/protocol/packp"
	"gopkg.in/src-d/go-git.v4/plumbing/protocol/packp/capability"
	"gopkg.in/src-d/go-git.v4/plumbing/protocol/packp/sideband"
	"gopkg.in/src-d/go-git.v4/plumbing/revlist"
	"gopkg.in/src-d/go-git.v4/plumbing/storer"
	"gopkg.in/src-d/go-git.v4/plumbing/transport"
	"gopkg.in/src-d/go-git.v4/plumbing/transport/client"
	"gopkg.in/src-d/go-git.v4/storage"
	"gopkg.in/src-d/go-git.v4/storage/filesystem"
	"gopkg.in/src-d/go-git.v4/storage/memory"
	"gopkg.in/src-d/go-git.v4/utils/ioutil"
)

var (
	NoErrAlreadyUpToDate     = errors.New("already up-to-date")
	ErrDeleteRefNotSupported = errors.New("server does not support delete-refs")
	ErrForceNeeded           = errors.New("some refs were not updated")
)

const (
	// This describes the maximum number of commits to walk when
	// computing the haves to send to a server, for each ref in the
	// repo containing this remote, when not using the multi-ack
	// protocol.  Setting this to 0 means there is no limit.
	maxHavesToVisitPerRef = 100
)

// Remote represents a connection to a remote repository.
type Remote struct {
	c *config.RemoteConfig
	s storage.Storer
}

func newRemote(s storage.Storer, c *config.RemoteConfig) *Remote {
	return &Remote{s: s, c: c}
}

// Config returns the RemoteConfig object used to instantiate this Remote.
func (r *Remote) Config() *config.RemoteConfig {
	return r.c
}

func (r *Remote) String() string {
	var fetch, push string
	if len(r.c.URLs) > 0 {
		fetch = r.c.URLs[0]
		push = r.c.URLs[0]
	}

	return fmt.Sprintf("%s\t%s (fetch)\n%[1]s\t%[3]s (push)", r.c.Name, fetch, push)
}

// Push performs a push to the remote. Returns NoErrAlreadyUpToDate if the
// remote was already up-to-date.
func (r *Remote) Push(o *PushOptions) error {
	return r.PushContext(context.Background(), o)
}

// PushContext performs a push to the remote. Returns NoErrAlreadyUpToDate if
// the remote was already up-to-date.
//
// The provided Context must be non-nil. If the context expires before the
// operation is complete, an error is returned. The context only affects to the
// transport operations.
func (r *Remote) PushContext(ctx context.Context, o *PushOptions) (err error) {
	if err := o.Validate(); err != nil {
		return err
	}

	if o.RemoteName != r.c.Name {
		return fmt.Errorf("remote names don't match: %s != %s", o.RemoteName, r.c.Name)
	}

	s, err := newSendPackSession(r.c.URLs[0], o.Auth)
	if err != nil {
		return err
	}

	defer ioutil.CheckClose(s, &err)

	ar, err := s.AdvertisedReferences()
	if err != nil {
		return err
	}

	remoteRefs, err := ar.AllReferences()
	if err != nil {
		return err
	}

	isDelete := false
	allDelete := true
	for _, rs := range o.RefSpecs {
		if rs.IsDelete() {
			isDelete = true
		} else {
			allDelete = false
		}
		if isDelete && !allDelete {
			break
		}
	}

	if isDelete && !ar.Capabilities.Supports(capability.DeleteRefs) {
		return ErrDeleteRefNotSupported
	}

	localRefs, err := r.references()
	if err != nil {
		return err
	}

	req, err := r.newReferenceUpdateRequest(o, localRefs, remoteRefs, ar)
	if err != nil {
		return err
	}

	if len(req.Commands) == 0 {
		return NoErrAlreadyUpToDate
	}

	objects := objectsToPush(req.Commands)

	haves, err := referencesToHashes(remoteRefs)
	if err != nil {
		return err
	}

	stop, err := r.s.Shallow()
	if err != nil {
		return err
	}

	// if we have shallow we should include this as part of the objects that
	// we are aware.
	haves = append(haves, stop...)

	var hashesToPush []plumbing.Hash
	// Avoid the expensive revlist operation if we're only doing deletes.
	if !allDelete {
		if r.c.IsFirstURLLocal() {
			// If we're are pushing to a local repo, it might be much
			// faster to use a local storage layer to get the commits
			// to ignore, when calculating the object revlist.
			localStorer := filesystem.NewStorage(
				osfs.New(r.c.URLs[0]), cache.NewObjectLRUDefault())
			hashesToPush, err = revlist.ObjectsWithStorageForIgnores(
				r.s, localStorer, objects, haves)
		} else {
			hashesToPush, err = revlist.Objects(r.s, objects, haves)
		}
		if err != nil {
			return err
		}
	}

	rs, err := pushHashes(ctx, s, r.s, req, hashesToPush, r.useRefDeltas(ar))
	if err != nil {
		return err
	}

	if err = rs.Error(); err != nil {
		return err
	}

	return r.updateRemoteReferenceStorage(req, rs)
}

func (r *Remote) useRefDeltas(ar *packp.AdvRefs) bool {
	return !ar.Capabilities.Supports(capability.OFSDelta)
}

func (r *Remote) newReferenceUpdateRequest(
	o *PushOptions,
	localRefs []*plumbing.Reference,
	remoteRefs storer.ReferenceStorer,
	ar *packp.AdvRefs,
) (*packp.ReferenceUpdateRequest, error) {
	req := packp.NewReferenceUpdateRequestFromCapabilities(ar.Capabilities)

	if o.Progress != nil {
		req.Progress = o.Progress
		if ar.Capabilities.Supports(capability.Sideband64k) {
			req.Capabilities.Set(capability.Sideband64k)
		} else if ar.Capabilities.Supports(capability.Sideband) {
			req.Capabilities.Set(capability.Sideband)
		}
	}

	if err := r.addReferencesToUpdate(o.RefSpecs, localRefs, remoteRefs, req); err != nil {
		return nil, err
	}

	return req, nil
}

func (r *Remote) updateRemoteReferenceStorage(
	req *packp.ReferenceUpdateRequest,
	result *packp.ReportStatus,
) error {

	for _, spec := range r.c.Fetch {
		for _, c := range req.Commands {
			if !spec.Match(c.Name) {
				continue
			}

			local := spec.Dst(c.Name)
			ref := plumbing.NewHashReference(local, c.New)
			switch c.Action() {
			case packp.Create, packp.Update:
				if err := r.s.SetReference(ref); err != nil {
					return err
				}
			case packp.Delete:
				if err := r.s.RemoveReference(local); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

// FetchContext fetches references along with the objects necessary to complete
// their histories.
//
// Returns nil if the operation is successful, NoErrAlreadyUpToDate if there are
// no changes to be fetched, or an error.
//
// The provided Context must be non-nil. If the context expires before the
// operation is complete, an error is returned. The context only affects to the
// transport operations.
func (r *Remote) FetchContext(ctx context.Context, o *FetchOptions) error {
	_, err := r.fetch(ctx, o)
	return err
}

// Fetch fetches references along with the objects necessary to complete their
// histories.
//
// Returns nil if the operation is successful, NoErrAlreadyUpToDate if there are
// no changes to be fetched, or an error.
func (r *Remote) Fetch(o *FetchOptions) error {
	return r.FetchContext(context.Background(), o)
}

func (r *Remote) fetch(ctx context.Context, o *FetchOptions) (sto storer.ReferenceStorer, err error) {
	if o.RemoteName == "" {
		o.RemoteName = r.c.Name
	}

	if err = o.Validate(); err != nil {
		return nil, err
	}

	if len(o.RefSpecs) == 0 {
		o.RefSpecs = r.c.Fetch
	}

	s, err := newUploadPackSession(r.c.URLs[0], o.Auth)
	if err != nil {
		return nil, err
	}

	defer ioutil.CheckClose(s, &err)

	ar, err := s.AdvertisedReferences()
	if err != nil {
		return nil, err
	}

	req, err := r.newUploadPackRequest(o, ar)
	if err != nil {
		return nil, err
	}

	remoteRefs, err := ar.AllReferences()
	if err != nil {
		return nil, err
	}

	localRefs, err := r.references()
	if err != nil {
		return nil, err
	}

	refs, err := calculateRefs(o.RefSpecs, remoteRefs, o.Tags)
	if err != nil {
		return nil, err
	}

	req.Wants, err = getWants(r.s, refs)
	if len(req.Wants) > 0 {
		req.Haves, err = getHaves(localRefs, remoteRefs, r.s)
		if err != nil {
			return nil, err
		}

		if err = r.fetchPack(ctx, o, s, req); err != nil {
			return nil, err
		}
	}

	updated, err := r.updateLocalReferenceStorage(o.RefSpecs, refs, remoteRefs, o.Tags, o.Force)
	if err != nil {
		return nil, err
	}

	if !updated {
		return remoteRefs, NoErrAlreadyUpToDate
	}

	return remoteRefs, nil
}

func newUploadPackSession(url string, auth transport.AuthMethod) (transport.UploadPackSession, error) {
	c, ep, err := newClient(url)
	if err != nil {
		return nil, err
	}

	return c.NewUploadPackSession(ep, auth)
}

func newSendPackSession(url string, auth transport.AuthMethod) (transport.ReceivePackSession, error) {
	c, ep, err := newClient(url)
	if err != nil {
		return nil, err
	}

	return c.NewReceivePackSession(ep, auth)
}

func newClient(url string) (transport.Transport, *transport.Endpoint, error) {
	ep, err := transport.NewEndpoint(url)
	if err != nil {
		return nil, nil, err
	}

	c, err := client.NewClient(ep)
	if err != nil {
		return nil, nil, err
	}

	return c, ep, err
}

func (r *Remote) fetchPack(ctx context.Context, o *FetchOptions, s transport.UploadPackSession,
	req *packp.UploadPackRequest) (err error) {

	reader, err := s.UploadPack(ctx, req)
	if err != nil {
		return err
	}

	defer ioutil.CheckClose(reader, &err)

	if err = r.updateShallow(o, reader); err != nil {
		return err
	}

	if err = packfile.UpdateObjectStorage(r.s,
		buildSidebandIfSupported(req.Capabilities, reader, o.Progress),
	); err != nil {
		return err
	}

	return err
}

func (r *Remote) addReferencesToUpdate(
	refspecs []config.RefSpec,
	localRefs []*plumbing.Reference,
	remoteRefs storer.ReferenceStorer,
	req *packp.ReferenceUpdateRequest,
) error {
	// This references dictionary will be used to search references by name.
	refsDict := make(map[string]*plumbing.Reference)
	for _, ref := range localRefs {
		refsDict[ref.Name().String()] = ref
	}

	for _, rs := range refspecs {
		if rs.IsDelete() {
			if err := r.deleteReferences(rs, remoteRefs, req); err != nil {
				return err
			}
		} else {
			err := r.addOrUpdateReferences(rs, localRefs, refsDict, remoteRefs, req)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (r *Remote) addOrUpdateReferences(
	rs config.RefSpec,
	localRefs []*plumbing.Reference,
	refsDict map[string]*plumbing.Reference,
	remoteRefs storer.ReferenceStorer,
	req *packp.ReferenceUpdateRequest,
) error {
	// If it is not a wilcard refspec we can directly search for the reference
	// in the references dictionary.
	if !rs.IsWildcard() {
		ref, ok := refsDict[rs.Src()]
		if !ok {
			return nil
		}

		return r.addReferenceIfRefSpecMatches(rs, remoteRefs, ref, req)
	}

	for _, ref := range localRefs {
		err := r.addReferenceIfRefSpecMatches(rs, remoteRefs, ref, req)
		if err != nil {
			return err
		}
	}

	return nil
}

func (r *Remote) deleteReferences(rs config.RefSpec,
	remoteRefs storer.ReferenceStorer, req *packp.ReferenceUpdateRequest) error {
	iter, err := remoteRefs.IterReferences()
	if err != nil {
		return err
	}

	return iter.ForEach(func(ref *plumbing.Reference) error {
		if ref.Type() != plumbing.HashReference {
			return nil
		}

		if rs.Dst("") != ref.Name() {
			return nil
		}

		cmd := &packp.Command{
			Name: ref.Name(),
			Old:  ref.Hash(),
			New:  plumbing.ZeroHash,
		}
		req.Commands = append(req.Commands, cmd)
		return nil
	})
}

func (r *Remote) addReferenceIfRefSpecMatches(rs config.RefSpec,
	remoteRefs storer.ReferenceStorer, localRef *plumbing.Reference,
	req *packp.ReferenceUpdateRequest) error {

	if localRef.Type() != plumbing.HashReference {
		return nil
	}

	if !rs.Match(localRef.Name()) {
		return nil
	}

	cmd := &packp.Command{
		Name: rs.Dst(localRef.Name()),
		Old:  plumbing.ZeroHash,
		New:  localRef.Hash(),
	}

	remoteRef, err := remoteRefs.Reference(cmd.Name)
	if err == nil {
		if remoteRef.Type() != plumbing.HashReference {
			//TODO: check actual git behavior here
			return nil
		}

		cmd.Old = remoteRef.Hash()
	} else if err != plumbing.ErrReferenceNotFound {
		return err
	}

	if cmd.Old == cmd.New {
		return nil
	}

	if !rs.IsForceUpdate() {
		if err := checkFastForwardUpdate(r.s, remoteRefs, cmd); err != nil {
			return err
		}
	}

	req.Commands = append(req.Commands, cmd)
	return nil
}

func (r *Remote) references() ([]*plumbing.Reference, error) {
	var localRefs []*plumbing.Reference
	iter, err := r.s.IterReferences()
	if err != nil {
		return nil, err
	}

	for {
		ref, err := iter.Next()
		if err == io.EOF {
			break
		}

		if err != nil {
			return nil, err
		}

		localRefs = append(localRefs, ref)
	}

	return localRefs, nil
}

func getRemoteRefsFromStorer(remoteRefStorer storer.ReferenceStorer) (
	map[plumbing.Hash]bool, error) {
	remoteRefs := map[plumbing.Hash]bool{}
	iter, err := remoteRefStorer.IterReferences()
	if err != nil {
		return nil, err
	}
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		if ref.Type() != plumbing.HashReference {
			return nil
		}
		remoteRefs[ref.Hash()] = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	return remoteRefs, nil
}

// getHavesFromRef populates the given `haves` map with the given
// reference, and up to `maxHavesToVisitPerRef` ancestor commits.
func getHavesFromRef(
	ref *plumbing.Reference,
	remoteRefs map[plumbing.Hash]bool,
	s storage.Storer,
	haves map[plumbing.Hash]bool,
) error {
	h := ref.Hash()
	if haves[h] {
		return nil
	}

	// No need to load the commit if we know the remote already
	// has this hash.
	if remoteRefs[h] {
		haves[h] = true
		return nil
	}

	commit, err := object.GetCommit(s, h)
	if err != nil {
		// Ignore the error if this isn't a commit.
		haves[ref.Hash()] = true
		return nil
	}

	// Until go-git supports proper commit negotiation during an
	// upload pack request, include up to `maxHavesToVisitPerRef`
	// commits from the history of each ref.
	walker := object.NewCommitPreorderIter(commit, haves, nil)
	toVisit := maxHavesToVisitPerRef
	return walker.ForEach(func(c *object.Commit) error {
		haves[c.Hash] = true
		toVisit--
		// If toVisit starts out at 0 (indicating there is no
		// max), then it will be negative here and we won't stop
		// early.
		if toVisit == 0 || remoteRefs[c.Hash] {
			return storer.ErrStop
		}
		return nil
	})
}

func getHaves(
	localRefs []*plumbing.Reference,
	remoteRefStorer storer.ReferenceStorer,
	s storage.Storer,
) ([]plumbing.Hash, error) {
	haves := map[plumbing.Hash]bool{}

	// Build a map of all the remote references, to avoid loading too
	// many parent commits for references we know don't need to be
	// transferred.
	remoteRefs, err := getRemoteRefsFromStorer(remoteRefStorer)
	if err != nil {
		return nil, err
	}

	for _, ref := range localRefs {
		if haves[ref.Hash()] {
			continue
		}

		if ref.Type() != plumbing.HashReference {
			continue
		}

		err = getHavesFromRef(ref, remoteRefs, s, haves)

		// Take care of the edge case where iterating using Preorder over
		// commits produces an `object not found` error, if using a shallow
		// clone with incomplete history.
		if err != nil && err != plumbing.ErrObjectNotFound {
			return nil, err
		}
	}

	var result []plumbing.Hash
	for h := range haves {
		result = append(result, h)
	}

	return result, nil
}

const refspecAllTags = "+refs/tags/*:refs/tags/*"

func calculateRefs(
	spec []config.RefSpec,
	remoteRefs storer.ReferenceStorer,
	tagMode TagMode,
) (memory.ReferenceStorage, error) {
	if tagMode == AllTags {
		spec = append(spec, refspecAllTags)
	}

	refs := make(memory.ReferenceStorage)
	for _, s := range spec {
		if err := doCalculateRefs(s, remoteRefs, refs); err != nil {
			return nil, err
		}
	}

	return refs, nil
}

func doCalculateRefs(
	s config.RefSpec,
	remoteRefs storer.ReferenceStorer,
	refs memory.ReferenceStorage,
) error {
	iter, err := remoteRefs.IterReferences()
	if err != nil {
		return err
	}

	var matched bool
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		if !s.Match(ref.Name()) {
			return nil
		}

		if ref.Type() == plumbing.SymbolicReference {
			target, err := storer.ResolveReference(remoteRefs, ref.Name())
			if err != nil {
				return err
			}

			ref = plumbing.NewHashReference(ref.Name(), target.Hash())
		}

		if ref.Type() != plumbing.HashReference {
			return nil
		}

		matched = true
		if err := refs.SetReference(ref); err != nil {
			return err
		}

		if !s.IsWildcard() {
			return storer.ErrStop
		}

		return nil
	})

	if !matched && !s.IsWildcard() {
		return fmt.Errorf("couldn't find remote ref %q", s.Src())
	}

	return err
}

func getWants(localStorer storage.Storer, refs memory.ReferenceStorage) ([]plumbing.Hash, error) {
	wants := map[plumbing.Hash]bool{}
	for _, ref := range refs {
		hash := ref.Hash()
		exists, err := objectExists(localStorer, ref.Hash())
		if err != nil {
			return nil, err
		}

		if !exists {
			wants[hash] = true
		}
	}

	var result []plumbing.Hash
	for h := range wants {
		result = append(result, h)
	}

	return result, nil
}

func objectExists(s storer.EncodedObjectStorer, h plumbing.Hash) (bool, error) {
	_, err := s.EncodedObject(plumbing.AnyObject, h)
	if err == plumbing.ErrObjectNotFound {
		return false, nil
	}

	return true, err
}

func checkFastForwardUpdate(s storer.EncodedObjectStorer, remoteRefs storer.ReferenceStorer, cmd *packp.Command) error {
	if cmd.Old == plumbing.ZeroHash {
		_, err := remoteRefs.Reference(cmd.Name)
		if err == plumbing.ErrReferenceNotFound {
			return nil
		}

		if err != nil {
			return err
		}

		return fmt.Errorf("non-fast-forward update: %s", cmd.Name.String())
	}

	ff, err := isFastForward(s, cmd.Old, cmd.New)
	if err != nil {
		return err
	}

	if !ff {
		return fmt.Errorf("non-fast-forward update: %s", cmd.Name.String())
	}

	return nil
}

func isFastForward(s storer.EncodedObjectStorer, old, new plumbing.Hash) (bool, error) {
	c, err := object.GetCommit(s, new)
	if err != nil {
		return false, err
	}

	found := false
	iter := object.NewCommitPreorderIter(c, nil, nil)
	err = iter.ForEach(func(c *object.Commit) error {
		if c.Hash != old {
			return nil
		}

		found = true
		return storer.ErrStop
	})
	return found, err
}

func (r *Remote) newUploadPackRequest(o *FetchOptions,
	ar *packp.AdvRefs) (*packp.UploadPackRequest, error) {

	req := packp.NewUploadPackRequestFromCapabilities(ar.Capabilities)

	if o.Depth != 0 {
		req.Depth = packp.DepthCommits(o.Depth)
		if err := req.Capabilities.Set(capability.Shallow); err != nil {
			return nil, err
		}
	}

	if o.Progress == nil && ar.Capabilities.Supports(capability.NoProgress) {
		if err := req.Capabilities.Set(capability.NoProgress); err != nil {
			return nil, err
		}
	}

	isWildcard := true
	for _, s := range o.RefSpecs {
		if !s.IsWildcard() {
			isWildcard = false
			break
		}
	}

	if isWildcard && o.Tags == TagFollowing && ar.Capabilities.Supports(capability.IncludeTag) {
		if err := req.Capabilities.Set(capability.IncludeTag); err != nil {
			return nil, err
		}
	}

	return req, nil
}

func buildSidebandIfSupported(l *capability.List, reader io.Reader, p sideband.Progress) io.Reader {
	var t sideband.Type

	switch {
	case l.Supports(capability.Sideband):
		t = sideband.Sideband
	case l.Supports(capability.Sideband64k):
		t = sideband.Sideband64k
	default:
		return reader
	}

	d := sideband.NewDemuxer(t, reader)
	d.Progress = p

	return d
}

func (r *Remote) updateLocalReferenceStorage(
	specs []config.RefSpec,
	fetchedRefs, remoteRefs memory.ReferenceStorage,
	tagMode TagMode,
	force bool,
) (updated bool, err error) {
	isWildcard := true
	forceNeeded := false

	for _, spec := range specs {
		if !spec.IsWildcard() {
			isWildcard = false
		}

		for _, ref := range fetchedRefs {
			if !spec.Match(ref.Name()) {
				continue
			}

			if ref.Type() != plumbing.HashReference {
				continue
			}

			localName := spec.Dst(ref.Name())
			old, _ := storer.ResolveReference(r.s, localName)
			new := plumbing.NewHashReference(localName, ref.Hash())

			// If the ref exists locally as a branch and force is not specified,
			// only update if the new ref is an ancestor of the old
			if old != nil && old.Name().IsBranch() && !force && !spec.IsForceUpdate() {
				ff, err := isFastForward(r.s, old.Hash(), new.Hash())
				if err != nil {
					return updated, err
				}

				if !ff {
					forceNeeded = true
					continue
				}
			}

			refUpdated, err := checkAndUpdateReferenceStorerIfNeeded(r.s, new, old)
			if err != nil {
				return updated, err
			}

			if refUpdated {
				updated = true
			}
		}
	}

	if tagMode == NoTags {
		return updated, nil
	}

	tags := fetchedRefs
	if isWildcard {
		tags = remoteRefs
	}
	tagUpdated, err := r.buildFetchedTags(tags)
	if err != nil {
		return updated, err
	}

	if tagUpdated {
		updated = true
	}

	if err == nil && forceNeeded {
		err = ErrForceNeeded
	}

	return
}

func (r *Remote) buildFetchedTags(refs memory.ReferenceStorage) (updated bool, err error) {
	for _, ref := range refs {
		if !ref.Name().IsTag() {
			continue
		}

		_, err := r.s.EncodedObject(plumbing.AnyObject, ref.Hash())
		if err == plumbing.ErrObjectNotFound {
			continue
		}

		if err != nil {
			return false, err
		}

		refUpdated, err := updateReferenceStorerIfNeeded(r.s, ref)
		if err != nil {
			return updated, err
		}

		if refUpdated {
			updated = true
		}
	}

	return
}

// List the references on the remote repository.
func (r *Remote) List(o *ListOptions) (rfs []*plumbing.Reference, err error) {
	s, err := newUploadPackSession(r.c.URLs[0], o.Auth)
	if err != nil {
		return nil, err
	}

	defer ioutil.CheckClose(s, &err)

	ar, err := s.AdvertisedReferences()
	if err != nil {
		return nil, err
	}

	allRefs, err := ar.AllReferences()
	if err != nil {
		return nil, err
	}

	refs, err := allRefs.IterReferences()
	if err != nil {
		return nil, err
	}

	var resultRefs []*plumbing.Reference
	refs.ForEach(func(ref *plumbing.Reference) error {
		resultRefs = append(resultRefs, ref)
		return nil
	})

	return resultRefs, nil
}

func objectsToPush(commands []*packp.Command) []plumbing.Hash {
	var objects []plumbing.Hash
	for _, cmd := range commands {
		if cmd.New == plumbing.ZeroHash {
			continue
		}

		objects = append(objects, cmd.New)
	}
	return objects
}

func referencesToHashes(refs storer.ReferenceStorer) ([]plumbing.Hash, error) {
	iter, err := refs.IterReferences()
	if err != nil {
		return nil, err
	}

	var hs []plumbing.Hash
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		if ref.Type() != plumbing.HashReference {
			return nil
		}

		hs = append(hs, ref.Hash())
		return nil
	})
	if err != nil {
		return nil, err
	}

	return hs, nil
}

func pushHashes(
	ctx context.Context,
	sess transport.ReceivePackSession,
	s storage.Storer,
	req *packp.ReferenceUpdateRequest,
	hs []plumbing.Hash,
	useRefDeltas bool,
) (*packp.ReportStatus, error) {

	rd, wr := io.Pipe()
	req.Packfile = rd
	config, err := s.Config()
	if err != nil {
		return nil, err
	}

	// Set buffer size to 1 so the error message can be written when
	// ReceivePack fails. Otherwise the goroutine will be blocked writing
	// to the channel.
	done := make(chan error, 1)

	go func() {
		e := packfile.NewEncoder(wr, s, useRefDeltas)
		if _, err := e.Encode(hs, config.Pack.Window); err != nil {
			done <- wr.CloseWithError(err)
			return
		}

		done <- wr.Close()
	}()

	rs, err := sess.ReceivePack(ctx, req)
	if err != nil {
		// close the pipe to unlock encode write
		_ = rd.Close()
		return nil, err
	}

	if err := <-done; err != nil {
		return nil, err
	}

	return rs, nil
}

func (r *Remote) updateShallow(o *FetchOptions, resp *packp.UploadPackResponse) error {
	if o.Depth == 0 || len(resp.Shallows) == 0 {
		return nil
	}

	shallows, err := r.s.Shallow()
	if err != nil {
		return err
	}

outer:
	for _, s := range resp.Shallows {
		for _, oldS := range shallows {
			if s == oldS {
				continue outer
			}
		}
		shallows = append(shallows, s)
	}

	return r.s.SetShallow(shallows)
}
