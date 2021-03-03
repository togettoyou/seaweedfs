package command

import (
	"context"
	"fmt"
	"github.com/chrislusf/seaweedfs/weed/filer"
	"github.com/chrislusf/seaweedfs/weed/glog"
	"github.com/spf13/viper"
	"google.golang.org/grpc"
	"io"
	"reflect"
	"time"

	"github.com/chrislusf/seaweedfs/weed/pb"
	"github.com/chrislusf/seaweedfs/weed/pb/filer_pb"
	"github.com/chrislusf/seaweedfs/weed/security"
	"github.com/chrislusf/seaweedfs/weed/util"
)

var (
	metaBackup FilerMetaBackupOptions
)

type FilerMetaBackupOptions struct {
	grpcDialOption    grpc.DialOption
	filerAddress      *string
	filerDirectory    *string
	restart           *bool
	backupFilerConfig *string

	store filer.FilerStore
}

func init() {
	cmdFilerMetaBackup.Run = runFilerMetaBackup // break init cycle
	metaBackup.filerAddress = cmdFilerMetaBackup.Flag.String("filer", "localhost:8888", "filer hostname:port")
	metaBackup.filerDirectory = cmdFilerMetaBackup.Flag.String("filerDir", "/", "a folder on the filer")
	metaBackup.restart = cmdFilerMetaBackup.Flag.Bool("restart", false, "copy the full metadata snapshot before async continuous backup")
	metaBackup.backupFilerConfig = cmdFilerMetaBackup.Flag.String("config", "", "path to filer.toml specifying backup filer store")
}

var cmdFilerMetaBackup = &Command{
	UsageLine: "filer.meta.backup [-filer=localhost:8888] [-filerDir=/] [-restart] -config=/path/to/backup_filer.toml",
	Short:     "continuously backup filer meta data changes to anther filer store specified in a backup_filer.toml",
	Long: `continuously backup filer meta data changes. 
The backup writes to another filer store specified in a backup_filer.toml.

	weed filer.meta.backup -config=/path/to/backup_filer.toml -filer="localhost:8888"
	weed filer.meta.backup -config=/path/to/backup_filer.toml -filer="localhost:8888" -restart

  `,
}

func runFilerMetaBackup(cmd *Command, args []string) bool {

	metaBackup.grpcDialOption = security.LoadClientTLS(util.GetViper(), "grpc.client")

	// load backup_filer.toml
	v := viper.New()
	v.SetConfigFile(*metaBackup.backupFilerConfig)

	if err := v.ReadInConfig(); err != nil { // Handle errors reading the config file
		glog.Fatalf("Failed to load %s file.\nPlease use this command to generate the a %s.toml file\n"+
			"    weed scaffold -config=%s -output=.\n\n\n",
			*metaBackup.backupFilerConfig, "backup_filer", "filer")
	}

	if err := metaBackup.initStore(v); err != nil {
		glog.V(0).Infof("init backup filer store: %v", err)
		return true
	}

	missingPreviousBackup := false
	_, err := metaBackup.getOffset()
	if err != nil {
		missingPreviousBackup = true
	}

	if *metaBackup.restart || missingPreviousBackup {
		glog.V(0).Infof("traversing metadata tree...")
		startTime := time.Now()
		if err := metaBackup.traverseMetadata(); err != nil {
			glog.Errorf("traverse meta data: %v", err)
			return true
		}
		glog.V(0).Infof("metadata copied up to %v", startTime)
		if err := metaBackup.setOffset(startTime); err != nil {
			startTime = time.Now()
		}
	}

	for {
		err := metaBackup.streamMetadataBackup()
		if err != nil {
			glog.Errorf("filer meta backup from %s: %v", *metaBackup.filerAddress, err)
			time.Sleep(1747 * time.Millisecond)
		}
	}

	return true
}

func (metaBackup *FilerMetaBackupOptions) initStore(v *viper.Viper) error {
	// load configuration for default filer store
	hasDefaultStoreConfigured := false
	for _, store := range filer.Stores {
		if v.GetBool(store.GetName() + ".enabled") {
			store = reflect.New(reflect.ValueOf(store).Elem().Type()).Interface().(filer.FilerStore)
			if err := store.Initialize(v, store.GetName()+"."); err != nil {
				glog.Fatalf("failed to initialize store for %s: %+v", store.GetName(), err)
			}
			glog.V(0).Infof("configured filer store to %s", store.GetName())
			hasDefaultStoreConfigured = true
			metaBackup.store = filer.NewFilerStoreWrapper(store)
			break
		}
	}
	if !hasDefaultStoreConfigured {
		return fmt.Errorf("no filer store enabled in %s", v.ConfigFileUsed())
	}

	return nil
}

func (metaBackup *FilerMetaBackupOptions) traverseMetadata() (err error) {
	var saveErr error

	traverseErr := filer_pb.TraverseBfs(metaBackup, util.FullPath(*metaBackup.filerDirectory), func(parentPath util.FullPath, entry *filer_pb.Entry) {

		println("+", parentPath.Child(entry.Name))
		if err := metaBackup.store.InsertEntry(context.Background(), filer.FromPbEntry(string(parentPath), entry)); err != nil {
			saveErr = fmt.Errorf("insert entry error: %v\n", err)
			return
		}

	})

	if traverseErr != nil {
		return fmt.Errorf("traverse: %v", traverseErr)
	}
	return saveErr
}

var (
	MetaBackupKey = []byte("metaBackup")
)

func (metaBackup *FilerMetaBackupOptions) streamMetadataBackup() error {

	startTime, err := metaBackup.getOffset()
	if err != nil {
		startTime = time.Now()
	}
	glog.V(0).Infof("streaming from %v", startTime)

	store := metaBackup.store

	eachEntryFunc := func(resp *filer_pb.SubscribeMetadataResponse) error {

		ctx := context.Background()
		message := resp.EventNotification

		if message.OldEntry == nil && message.NewEntry == nil {
			return nil
		}
		if message.OldEntry == nil && message.NewEntry != nil {
			println("+", util.FullPath(message.NewParentPath).Child(message.NewEntry.Name))
			entry := filer.FromPbEntry(message.NewParentPath, message.NewEntry)
			return store.InsertEntry(ctx, entry)
		}
		if message.OldEntry != nil && message.NewEntry == nil {
			println("-", util.FullPath(resp.Directory).Child(message.OldEntry.Name))
			return store.DeleteEntry(ctx, util.FullPath(resp.Directory).Child(message.OldEntry.Name))
		}
		if message.OldEntry != nil && message.NewEntry != nil {
			if resp.Directory == message.NewParentPath && message.OldEntry.Name == message.NewEntry.Name {
				println("~", util.FullPath(message.NewParentPath).Child(message.NewEntry.Name))
				entry := filer.FromPbEntry(message.NewParentPath, message.NewEntry)
				return store.UpdateEntry(ctx, entry)
			}
			println("-", util.FullPath(resp.Directory).Child(message.OldEntry.Name))
			if err := store.DeleteEntry(ctx, util.FullPath(resp.Directory).Child(message.OldEntry.Name)); err != nil {
				return err
			}
			println("+", util.FullPath(message.NewParentPath).Child(message.NewEntry.Name))
			return store.InsertEntry(ctx, filer.FromPbEntry(message.NewParentPath, message.NewEntry))
		}

		return nil
	}

	tailErr := pb.WithFilerClient(*metaBackup.filerAddress, metaBackup.grpcDialOption, func(client filer_pb.SeaweedFilerClient) error {

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		stream, err := client.SubscribeMetadata(ctx, &filer_pb.SubscribeMetadataRequest{
			ClientName: "meta_backup",
			PathPrefix: *metaBackup.filerDirectory,
			SinceNs:    startTime.UnixNano(),
		})
		if err != nil {
			return fmt.Errorf("listen: %v", err)
		}

		var counter int64
		var lastWriteTime time.Time
		for {
			resp, listenErr := stream.Recv()
			if listenErr == io.EOF {
				return nil
			}
			if listenErr != nil {
				return listenErr
			}
			if err = eachEntryFunc(resp); err != nil {
				return err
			}

			counter++
			if lastWriteTime.Add(3 * time.Second).Before(time.Now()) {
				glog.V(0).Infof("meta backup %s progressed to %v %0.2f/sec", *metaBackup.filerAddress, time.Unix(0, resp.TsNs), float64(counter)/float64(3))
				counter = 0
				lastWriteTime = time.Now()
				if err2 := metaBackup.setOffset(lastWriteTime); err2 != nil {
					return err2
				}
			}

		}

	})
	return tailErr
}

func (metaBackup *FilerMetaBackupOptions) getOffset() (lastWriteTime time.Time, err error) {
	value, err := metaBackup.store.KvGet(context.Background(), MetaBackupKey)
	if err != nil {
		return
	}
	tsNs := util.BytesToUint64(value)

	return time.Unix(0, int64(tsNs)), nil
}

func (metaBackup *FilerMetaBackupOptions) setOffset(lastWriteTime time.Time) error {
	valueBuf := make([]byte, 8)
	util.Uint64toBytes(valueBuf, uint64(lastWriteTime.UnixNano()))

	if err := metaBackup.store.KvPut(context.Background(), MetaBackupKey, valueBuf); err != nil {
		return err
	}
	return nil
}

var _ = filer_pb.FilerClient(&FilerMetaBackupOptions{})

func (metaBackup *FilerMetaBackupOptions) WithFilerClient(fn func(filer_pb.SeaweedFilerClient) error) error {

	return pb.WithFilerClient(*metaBackup.filerAddress, metaBackup.grpcDialOption, func(client filer_pb.SeaweedFilerClient) error {
		return fn(client)
	})

}

func (metaBackup *FilerMetaBackupOptions) AdjustedUrl(location *filer_pb.Location) string {
	return location.Url
}
