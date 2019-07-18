package storage

import (
	"fmt"
	"path"

	"github.com/weaveworks/ignite/pkg/apis/ignite/scheme"
	meta "github.com/weaveworks/ignite/pkg/apis/meta/v1alpha1"
	"github.com/weaveworks/ignite/pkg/constants"
	"github.com/weaveworks/ignite/pkg/storage/serializer"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"
)

// Storage is an interface for persisting and retrieving API objects to/from a backend
// One Storage instance handles all different Kinds of Objects
type Storage interface {
	// New creates a new object for the specified kind
	New(gvk schema.GroupVersionKind) (meta.Object, error)
	// Get returns a new Object for the resource at the specified kind/uid path, based on the file content
	Get(gvk schema.GroupVersionKind, uid meta.UID) (meta.Object, error)
	// Set saves the Object to disk. If the object does not exist, the
	// ObjectMeta.Created field is set automatically
	Set(gvk schema.GroupVersionKind, obj meta.Object) error
	// Delete removes an object from the storage
	Delete(gvk schema.GroupVersionKind, uid meta.UID) error
	// List lists objects for the specific kind
	List(gvk schema.GroupVersionKind) ([]meta.Object, error)
	// ListMeta lists all objects' APIType representation. In other words,
	// only metadata about each object is unmarshalled (uid/name/kind/apiVersion).
	// This allows for faster runs (no need to unmarshal "the world"), and less
	// resource usage, when only metadata is unmarshalled into memory
	ListMeta(gvk schema.GroupVersionKind) ([]meta.Object, error)
	// Count returns the amount of available Objects of a specific kind
	// This is used by Caches to check if all objects are cached to perform a List
	Count(gvk schema.GroupVersionKind) (uint64, error)
}

// DefaultStorage is the default storage implementation
var DefaultStorage = NewCache(NewGenericStorage(NewDefaultRawStorage(constants.DATA_DIR), scheme.Serializer))

// NewGenericStorage constructs a new Storage
func NewGenericStorage(rawStorage RawStorage, serializer serializer.Serializer) Storage {
	return &GenericStorage{rawStorage, serializer}
}

// GenericStorage implements the Storage interface
type GenericStorage struct {
	raw        RawStorage
	serializer serializer.Serializer
}

var _ Storage = &GenericStorage{}

// New creates a new object for the specified kind
// TODO: Create better error handling if the GVK specified is not recognized
func (s *GenericStorage) New(gvk schema.GroupVersionKind) (meta.Object, error) {
	obj, err := s.serializer.Scheme().New(gvk)
	if err != nil {
		return nil, err
	}

	// Default either through the scheme, or the high-level serializer object
	if gvk.Version == runtime.APIVersionInternal {
		if err := s.serializer.DefaultInternal(obj); err != nil {
			return nil, err
		}
	} else {
		s.serializer.Scheme().Default(obj)
	}

	// Cast to meta.Object, and make sure it works
	metaObj, ok := obj.(meta.Object)
	if !ok {
		return nil, fmt.Errorf("can't convert to ignite object")
	}
	// Set the desired gvk from the caller of this object
	// In practice, this means, although we created an internal type,
	// from defaulting external TypeMeta information was set. Set the
	// desired gvk here so it's correctly handled in all code that gets
	// the gvk from the object
	metaObj.SetGroupVersionKind(gvk)
	return metaObj, nil
}

// Get returns a new Object for the resource at the specified kind/uid path, based on the file content
func (s *GenericStorage) Get(gvk schema.GroupVersionKind, uid meta.UID) (meta.Object, error) {
	storageKey := KeyForUID(gvk, uid)
	content, err := s.raw.Read(storageKey)
	if err != nil {
		return nil, err
	}

	// Decode the bytes to the internal version of the object, if desired
	isInternal := gvk.Version == runtime.APIVersionInternal
	return s.decode(content, isInternal)
}

// Set saves the Object to disk
func (s *GenericStorage) Set(gvk schema.GroupVersionKind, obj meta.Object) error {
	b, err := s.serializer.EncodeJSON(obj)
	if err != nil {
		return err
	}

	storageKey := KeyForUID(gvk, obj.GetUID())
	return s.raw.Write(storageKey, b)
}

// Delete removes an object from the storage
func (s *GenericStorage) Delete(gvk schema.GroupVersionKind, uid meta.UID) error {
	storageKey := KeyForUID(gvk, uid)
	return s.raw.Delete(storageKey)
}

// List lists objects for the specific kind
func (s *GenericStorage) List(gvk schema.GroupVersionKind) (result []meta.Object, walkerr error) {
	walkerr = s.walkKind(gvk, func(content []byte) error {
		isInternal := gvk.Version == runtime.APIVersionInternal
		obj, err := s.decode(content, isInternal)
		if err != nil {
			return err
		}

		result = append(result, obj)
		return nil
	})
	return
}

// ListMeta lists all objects' APIType representation. In other words,
// only metadata about each object is unmarshalled (uid/name/kind/apiVersion).
// This allows for faster runs (no need to unmarshal "the world"), and less
// resource usage, when only metadata is unmarshalled into memory
func (s *GenericStorage) ListMeta(gvk schema.GroupVersionKind) (result []meta.Object, walkerr error) {
	walkerr = s.walkKind(gvk, func(content []byte) error {
		obj := meta.NewAPIType()
		// The yaml package supports both YAML and JSON
		if err := yaml.Unmarshal(content, obj); err != nil {
			return err
		}
		// Set the desired gvk from the caller of this object
		// In practice, this means, although we got an external type,
		// we might want internal objects later in the client. Hence,
		// set the right expectation here
		obj.SetGroupVersionKind(gvk)

		result = append(result, obj)
		return nil
	})
	return
}

// Count counts the objects for the specific kind
func (s *GenericStorage) Count(gvk schema.GroupVersionKind) (uint64, error) {
	entries, err := s.raw.List(KeyForKind(gvk))
	return uint64(len(entries)), err
}

func (s *GenericStorage) decode(content []byte, internal bool) (meta.Object, error) {
	// Decode the bytes to the internal version of the object
	obj, err := s.serializer.Decode(content, internal)
	if err != nil {
		return nil, err
	}
	// Cast to meta.Object, and make sure it works
	metaObj, ok := obj.(meta.Object)
	if !ok {
		return nil, fmt.Errorf("can't convert to ignite object")
	}
	return metaObj, nil
}

func (s *GenericStorage) walkKind(gvk schema.GroupVersionKind, fn func(content []byte) error) error {
	kindKey := KeyForKind(gvk)
	entries, err := s.raw.List(kindKey)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		// Allow metadata.json to not exist, although the directory does exist
		if !s.raw.Exists(entry) {
			continue
		}

		content, err := s.raw.Read(entry)
		if err != nil {
			return err
		}

		if err := fn(content); err != nil {
			return err
		}
	}

	return nil
}

func KeyForUID(gvk schema.GroupVersionKind, uid meta.UID) string {
	return "/" + path.Join(meta.Kind(gvk.Kind).Lower(), uid.String())
}

func KeyForKind(gvk schema.GroupVersionKind) string {
	return "/" + meta.Kind(gvk.Kind).Lower()
}
