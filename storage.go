package gitessera

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"log/slog"

	"github.com/google/go-github/v60/github"
	"github.com/transparency-dev/merkle/compact"
	"github.com/transparency-dev/merkle/rfc6962"
	"github.com/transparency-dev/tessera"
	"github.com/transparency-dev/tessera/api"
	"github.com/transparency-dev/tessera/api/layout"
	"golang.org/x/exp/maps"
)

// GitHubStorage implements tessera storage using GitHub API.
type GitHubStorage struct {
	client *github.Client
	owner  string
	repo   string
	branch string
}

// NewGitHubStorage creates a new GitHubStorage instance.
func NewGitHubStorage(client *github.Client, owner, repo, branch string) *GitHubStorage {
	return &GitHubStorage{
		client: client,
		owner:  owner,
		repo:   repo,
		branch: branch,
	}
}

func (s *GitHubStorage) Appender(ctx context.Context, opts *tessera.AppendOptions) (*tessera.Appender, tessera.LogReader, error) {
	newCP := opts.CheckpointPublisher(s, nil)

	return &tessera.Appender{
		Add: func(ctx context.Context, entry *tessera.Entry) tessera.IndexFuture {
			return func() (tessera.Index, error) {
				size, err := s.IntegratedSize(ctx)
				if err != nil {
					return tessera.Index{}, err
				}

				getTiles := func(ctx context.Context, tileIDs []TileID, treeSize uint64) ([]*api.HashTile, error) {
					res := make([]*api.HashTile, 0, len(tileIDs))
					for _, id := range tileIDs {
						b, err := s.ReadTile(ctx, id.Level, id.Index, layout.PartialTileSize(id.Level, id.Index, treeSize))
						slog.DebugContext(ctx, "Read tile", slog.Uint64("level", id.Level), slog.Uint64("index", id.Index), slog.Int("len", len(b)), slog.Any("error", err))
						if err != nil {
							if errors.Is(err, os.ErrNotExist) {
								res = append(res, nil)
								continue
							}
							return nil, err
						}
						if len(b)%32 != 0 {
							slog.ErrorContext(ctx, "Invalid tile length", slog.Uint64("level", id.Level), slog.Uint64("index", id.Index), slog.Int("len", len(b)))
							return nil, fmt.Errorf("invalid tile length %d is not a multiple of 32", len(b))
						}
						var tile api.HashTile
						for i := 0; i < len(b); i += 32 {
							tile.Nodes = append(tile.Nodes, b[i:i+32])
						}
						res = append(res, &tile)
					}
					return res, nil
				}

				leafHashes := [][]byte{entry.LeafHash()}
				newSize, newRoot, tiles, err := Integrate(ctx, getTiles, size, leafHashes)
				if err != nil {
					return tessera.Index{}, err
				}

				cpRaw, err := newCP(ctx, newSize, newRoot)
				if err != nil {
					return tessera.Index{}, err
				}

				// Prepare updates
				bundleIndex := size / layout.EntryBundleWidth
				entriesInBundle := size % layout.EntryBundleWidth
				newEntriesInBundle := entriesInBundle + 1

				var p uint8
				if newEntriesInBundle == layout.EntryBundleWidth {
					p = 0
				} else {
					p = uint8(newEntriesInBundle)
				}
				bundlePath := layout.EntriesPath(bundleIndex, p)

				var bundleData []byte
				if entriesInBundle > 0 {
					var err error
					bundleData, err = s.ReadEntryBundle(ctx, bundleIndex, uint8(entriesInBundle))
					if err != nil {
						return tessera.Index{}, err
					}
				}
				newEntryData := entry.MarshalBundleData(size)
				bundleData = append(bundleData, newEntryData...)

				// Git operations
				commitSHA, err := s.getBranchRef(ctx)
				if err != nil {
					return tessera.Index{}, err
				}
				treeSHA, err := s.getCommitTree(ctx, commitSHA)
				if err != nil {
					return tessera.Index{}, err
				}

				var entries []*github.TreeEntry

				// Create blobs and tree entries
				addBlob := func(path string, content []byte) error {
					sha, err := s.createBlob(ctx, content)
					if err != nil {
						return err
					}
					entries = append(entries, &github.TreeEntry{
						Path: github.String(path),
						Mode: github.String("100644"),
						Type: github.String("blob"),
						SHA:  github.String(sha),
					})
					return nil
				}

				if err := addBlob(layout.CheckpointPath, cpRaw); err != nil {
					return tessera.Index{}, err
				}
				if err := addBlob(bundlePath, bundleData); err != nil {
					return tessera.Index{}, err
				}
				for id, tile := range tiles {
					path := layout.TilePath(id.Level, id.Index, layout.PartialTileSize(id.Level, id.Index, newSize))
					
					slog.DebugContext(ctx, "Writing tile", slog.String("path", path), slog.Int("num_nodes", len(tile.Nodes)))
					for i, h := range tile.Nodes {
						slog.DebugContext(ctx, "Tile node", slog.Int("i", i), slog.Int("len", len(h)))
					}

					var buf bytes.Buffer
					for _, h := range tile.Nodes {
						buf.Write(h)
					}
					tileBytes := buf.Bytes()
					slog.DebugContext(ctx, "Wrote tile bytes", slog.String("path", path), slog.Int("len", len(tileBytes)))
					
					if err := addBlob(path, tileBytes); err != nil {
						return tessera.Index{}, err
					}
				}

				newTreeSHA, err := s.createTree(ctx, treeSHA, entries)
				if err != nil {
					return tessera.Index{}, err
				}
				newCommitSHA, err := s.createCommit(ctx, newTreeSHA, commitSHA, "Tessera: add entry")
				if err != nil {
					return tessera.Index{}, err
				}
				err = s.updateRef(ctx, newCommitSHA)
				if err != nil {
					return tessera.Index{}, err
				}

				return tessera.Index{Index: size}, nil
			}
		},
	}, s, nil
}

func (s *GitHubStorage) getBranchRef(ctx context.Context) (string, error) {
	ref, _, err := s.client.Git.GetRef(ctx, s.owner, s.repo, "refs/heads/"+s.branch)
	if err != nil {
		return "", err
	}
	return ref.Object.GetSHA(), nil
}

func (s *GitHubStorage) getCommitTree(ctx context.Context, commitSHA string) (string, error) {
	commit, _, err := s.client.Git.GetCommit(ctx, s.owner, s.repo, commitSHA)
	if err != nil {
		return "", err
	}
	return commit.Tree.GetSHA(), nil
}

func (s *GitHubStorage) createBlob(ctx context.Context, content []byte) (string, error) {
	blob := &github.Blob{
		Content:  github.String(base64.StdEncoding.EncodeToString(content)),
		Encoding: github.String("base64"),
	}
	res, _, err := s.client.Git.CreateBlob(ctx, s.owner, s.repo, blob)
	if err != nil {
		return "", err
	}
	return res.GetSHA(), nil
}

func (s *GitHubStorage) createTree(ctx context.Context, baseTreeSHA string, entries []*github.TreeEntry) (string, error) {
	res, _, err := s.client.Git.CreateTree(ctx, s.owner, s.repo, baseTreeSHA, entries)
	if err != nil {
		return "", err
	}
	return res.GetSHA(), nil
}

func (s *GitHubStorage) createCommit(ctx context.Context, treeSHA string, parentSHA string, message string) (string, error) {
	commit := &github.Commit{
		Tree:    &github.Tree{SHA: github.String(treeSHA)},
		Message: github.String(message),
		Parents: []*github.Commit{{SHA: github.String(parentSHA)}},
	}
	res, _, err := s.client.Git.CreateCommit(ctx, s.owner, s.repo, commit, nil)
	if err != nil {
		return "", err
	}
	return res.GetSHA(), nil
}

func (s *GitHubStorage) updateRef(ctx context.Context, commitSHA string) error {
	ref := &github.Reference{
		Ref:    github.String("refs/heads/" + s.branch),
		Object: &github.GitObject{SHA: github.String(commitSHA)},
	}
	_, _, err := s.client.Git.UpdateRef(ctx, s.owner, s.repo, ref, false)
	return err
}

// TileID represents a tile address in tile-space.
type TileID struct {
	Level uint64
	Index uint64
}

// SequencedEntry represents a log entry which has already been sequenced.
type SequencedEntry struct {
	// BundleData is the entry's data serialised into the correct format for appending to an entry bundle.
	BundleData []byte
	// LeafHash is the entry's Merkle leaf hash.
	LeafHash []byte
}

// Integrate handles integrating new leaf hashes into the log, and returns the new state.
func Integrate(ctx context.Context, getTiles func(ctx context.Context, tileIDs []TileID, treeSize uint64) ([]*api.HashTile, error), fromSize uint64, leafHashes [][]byte) (newSize uint64, rootHash []byte, tiles map[TileID]*api.HashTile, err error) {
	tb := newTreeBuilder(getTiles)
	return tb.integrate(ctx, fromSize, leafHashes)
}

// getPopulatedTileFunc is the signature of a function which can return a fully populated tile for the given tile coords.
type getPopulatedTileFunc func(ctx context.Context, tileID TileID, treeSize uint64) (*populatedTile, error)

// treeBuilder constructs Merkle trees.
type treeBuilder struct {
	readCache *tileReadCache
	rf        *compact.RangeFactory
}

// newTreeBuilder creates a new instance of treeBuilder.
func newTreeBuilder(getTiles func(ctx context.Context, tileIDs []TileID, treeSize uint64) ([]*api.HashTile, error)) *treeBuilder {
	readCache := newTileReadCache(getTiles)
	return &treeBuilder{
		readCache: &readCache,
		rf:        &compact.RangeFactory{Hash: rfc6962.DefaultHasher.HashChildren},
	}
}

// newRange creates a new compact.Range for the specified treeSize, fetching tiles as necessary.
func (t *treeBuilder) newRange(ctx context.Context, treeSize uint64) (*compact.Range, error) {
	rangeNodes := compact.RangeNodes(0, treeSize, nil)
	toFetch := make(map[TileID]struct{})
	for _, id := range rangeNodes {
		tLevel, tIndex, _, _ := layout.NodeCoordsToTileAddress(uint64(id.Level), id.Index)
		toFetch[TileID{Level: tLevel, Index: tIndex}] = struct{}{}
	}
	if err := t.readCache.Prewarm(ctx, maps.Keys(toFetch), treeSize); err != nil {
		return nil, fmt.Errorf("Prewarm: %v", err)
	}

	hashes := make([][]byte, 0, len(rangeNodes))
	for _, id := range rangeNodes {
		tLevel, tIndex, nLevel, nIndex := layout.NodeCoordsToTileAddress(uint64(id.Level), id.Index)
		ft, err := t.readCache.Get(ctx, TileID{Level: tLevel, Index: tIndex}, treeSize)
		if err != nil {
			return nil, err
		}
		h := ft.Get(compact.NodeID{Level: nLevel, Index: nIndex})
		if h == nil {
			return nil, fmt.Errorf("missing node: [%d/%d@%d]", id.Level, id.Index, treeSize)
		}
		hashes = append(hashes, h)
	}
	return t.rf.NewRange(0, treeSize, hashes)
}

func (t *treeBuilder) integrate(ctx context.Context, fromSize uint64, leafHashes [][]byte) (uint64, []byte, map[TileID]*api.HashTile, error) {
	slog.DebugContext(ctx, "integrate", slog.Uint64("fromSize", fromSize), slog.Int("numEntries", len(leafHashes)))

	baseRange, err := t.newRange(ctx, fromSize)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("failed to create range covering existing log: %w", err)
	}

	r, err := baseRange.GetRootHash(nil)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("invalid log state, unable to recalculate root: %w", err)
	}
	if len(leafHashes) == 0 {
		slog.DebugContext(ctx, "Nothing to do")
		if fromSize == 0 {
			r = rfc6962.DefaultHasher.EmptyRoot()
		}
		return fromSize, r, nil, nil
	}

	slog.DebugContext(ctx, "Loaded state", slog.String("hash", fmt.Sprintf("%x", r)))
	newRange := t.rf.NewEmptyRange(fromSize)
	tc := newTileWriteCache(fromSize, t.readCache.Get)
	visitor := tc.Visitor(ctx)
	for _, e := range leafHashes {
		if err := newRange.Append(e, visitor); err != nil {
			return 0, nil, nil, fmt.Errorf("newRange.Append(): %v", err)
		}
	}
	if err := tc.Err(); err != nil {
		return 0, nil, nil, err
	}

	if err := baseRange.AppendRange(newRange, visitor); err != nil {
		return 0, nil, nil, fmt.Errorf("failed to merge new range onto existing log: %w", err)
	}

	if err := tc.Err(); err != nil {
		return 0, nil, nil, err
	}

	newRoot, err := baseRange.GetRootHash(nil)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("failed to calculate new root hash: %w", err)
	}

	slog.DebugContext(ctx, "New log state", slog.String("size", fmt.Sprintf("%x", baseRange.End())), slog.String("hash", fmt.Sprintf("%x", newRoot)))

	return baseRange.End(), newRoot, tc.Tiles(), nil
}

type tileReadCache struct {
	entries  map[string]*populatedTile
	getTiles func(ctx context.Context, tileIDs []TileID, treeSize uint64) ([]*api.HashTile, error)
}

func newTileReadCache(getTiles func(ctx context.Context, tileIDs []TileID, treeSize uint64) ([]*api.HashTile, error)) tileReadCache {
	return tileReadCache{
		entries:  make(map[string]*populatedTile),
		getTiles: getTiles,
	}
}

func (r *tileReadCache) Get(ctx context.Context, tileID TileID, treeSize uint64) (*populatedTile, error) {
	k := layout.TilePath(uint64(tileID.Level), tileID.Index, layout.PartialTileSize(tileID.Level, tileID.Index, treeSize))
	e, ok := r.entries[k]
	if !ok {
		slog.DebugContext(ctx, "Readcache miss", slog.String("k", k))
		t, err := r.getTiles(ctx, []TileID{tileID}, treeSize)
		if err != nil {
			return nil, err
		}
		e, err = newPopulatedTile(t[0])
		if err != nil {
			return nil, fmt.Errorf("failed to create fulltile: %v", err)
		}
		r.entries[k] = e
	}
	return e, nil
}

func (r *tileReadCache) Prewarm(ctx context.Context, tileIDs []TileID, treeSize uint64) error {
	t, err := r.getTiles(ctx, tileIDs, treeSize)
	if err != nil {
		return err
	}
	for i, tile := range t {
		e, err := newPopulatedTile(tile)
		if err != nil {
			return fmt.Errorf("failed to create fulltile: %v", err)
		}
		k := layout.TilePath(uint64(tileIDs[i].Level), tileIDs[i].Index, layout.PartialTileSize(tileIDs[i].Level, tileIDs[i].Index, treeSize))
		r.entries[k] = e
	}
	return nil
}

type tileWriteCache struct {
	m        map[TileID]*populatedTile
	err      []error
	treeSize uint64
	getTile  getPopulatedTileFunc
}

func newTileWriteCache(treeSize uint64, getTile getPopulatedTileFunc) *tileWriteCache {
	return &tileWriteCache{
		m:        make(map[TileID]*populatedTile),
		treeSize: treeSize,
		getTile:  getTile,
	}
}

func (tc *tileWriteCache) Err() error {
	return errors.Join(tc.err...)
}

func minImpliedTreeSize(id TileID) uint64 {
	return (id.Index * layout.TileWidth) << (id.Level * 8)
}

func (tc *tileWriteCache) Visitor(ctx context.Context) compact.VisitFn {
	return func(id compact.NodeID, hash []byte) {
		tileLevel, tileIndex, nodeLevel, nodeIndex := layout.NodeCoordsToTileAddress(uint64(id.Level), uint64(id.Index))
		tileID := TileID{Level: tileLevel, Index: tileIndex}
		tile := tc.m[tileID]
		if tile == nil {
			var err error
			if iSize := minImpliedTreeSize(tileID); iSize <= tc.treeSize {
				tile, err = tc.getTile(ctx, tileID, tc.treeSize)
				if err != nil {
					tc.err = append(tc.err, err)
					return
				}
			}
			if tile == nil {
				tile, err = newPopulatedTile(nil)
				if err != nil {
					tc.err = append(tc.err, err)
					return
				}
			}
		}
		tc.m[tileID] = tile
		idx := compact.NodeID{Level: nodeLevel, Index: nodeIndex}
		tile.Set(idx, hash)
	}
}

func (tc *tileWriteCache) Tiles() map[TileID]*api.HashTile {
	newTiles := make(map[TileID]*api.HashTile)
	for k, t := range tc.m {
		newTiles[k] = &api.HashTile{Nodes: t.leaves}
	}
	return newTiles
}

type populatedTile struct {
	inner  map[compact.NodeID][]byte
	leaves [][]byte
}

func newPopulatedTile(h *api.HashTile) (*populatedTile, error) {
	ft := &populatedTile{
		inner:  make(map[compact.NodeID][]byte),
		leaves: make([][]byte, 0, layout.TileWidth),
	}

	if h != nil {
		r := (&compact.RangeFactory{Hash: rfc6962.DefaultHasher.HashChildren}).NewEmptyRange(0)
		for _, h := range h.Nodes {
			if err := r.Append(h, ft.Set); err != nil {
				return nil, fmt.Errorf("failed to append to range: %v", err)
			}
		}
	}
	return ft, nil
}

func (f *populatedTile) Set(id compact.NodeID, hash []byte) {
	if id.Level == 0 {
		if l, idx := uint64(len(f.leaves)), id.Index; idx >= l {
			f.leaves = append(f.leaves, make([][]byte, idx-l+1)...)
		}
		f.leaves[id.Index] = hash
	} else {
		f.inner[id] = hash
	}
}

func (f *populatedTile) Get(id compact.NodeID) []byte {
	if id.Level == 0 {
		if l := uint64(len(f.leaves)); id.Index >= l {
			return nil
		}
		return f.leaves[id.Index]
	}
	return f.inner[id]
}
func checkpointUnsafe(rawCp []byte) (string, uint64, []byte, error) {
	parts := bytes.SplitN(rawCp, []byte{'\n'}, 4)
	if want, got := 4, len(parts); want != got {
		return "", 0, nil, fmt.Errorf("invalid checkpoint: %q", rawCp)
	}
	origin := string(parts[0])
	sizeStr := string(parts[1])
	hashStr := string(parts[2])
	size, err := strconv.ParseUint(sizeStr, 10, 64)
	if err != nil {
		return "", 0, nil, fmt.Errorf("failed to turn checkpoint size of %q into uint64: %v", sizeStr, err)
	}
	hash, err := base64.StdEncoding.DecodeString(hashStr)
	if err != nil {
		return "", 0, nil, fmt.Errorf("failed to decode hash: %v", err)
	}
	return origin, size, hash, nil
}

// ReadCheckpoint returns the latest checkpoint available.
func (s *GitHubStorage) ReadCheckpoint(ctx context.Context) ([]byte, error) {
	return s.readFile(ctx, layout.CheckpointPath)
}

// ReadTile returns the raw marshalled tile at the given coordinates, if it exists.
func (s *GitHubStorage) ReadTile(ctx context.Context, level, index uint64, p uint8) ([]byte, error) {
	path := layout.TilePath(level, index, p)
	return s.readFile(ctx, path)
}

// ReadEntryBundle returns the raw marshalled leaf bundle at the given coordinates, if it exists.
func (s *GitHubStorage) ReadEntryBundle(ctx context.Context, index uint64, p uint8) ([]byte, error) {
	path := layout.EntriesPath(index, p)
	return s.readFile(ctx, path)
}

func (s *GitHubStorage) readFile(ctx context.Context, path string) ([]byte, error) {
	rc, _, err := s.client.Repositories.DownloadContents(ctx, s.owner, s.repo, path, &github.RepositoryContentGetOptions{Ref: s.branch})
	if err != nil {
		var errResp *github.ErrorResponse
		if errors.As(err, &errResp) && errResp.Response.StatusCode == 404 {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	defer rc.Close()

	b, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	slog.DebugContext(ctx, "readFile download", slog.String("path", path), slog.Int("len", len(b)))
	return b, nil
}

// IntegratedSize returns the current size of the integrated tree.
func (s *GitHubStorage) IntegratedSize(ctx context.Context) (uint64, error) {
	cp, err := s.ReadCheckpoint(ctx)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	_, size, _, err := checkpointUnsafe(cp)
	if err != nil {
		return 0, err
	}
	return size, nil
}

// NextIndex returns the first as-yet unassigned index.
func (s *GitHubStorage) NextIndex(ctx context.Context) (uint64, error) {
	return s.IntegratedSize(ctx)
}
