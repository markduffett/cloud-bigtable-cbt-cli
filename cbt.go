/*
Copyright 2015 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

// Command docs are in cbtdoc.go.

import (
	"bytes"
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"go/format"
	"io"
	"log"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"text/template"
	"time"

	"cloud.google.com/go/bigtable"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

var (
	oFlag = flag.String("o", "", "if set, redirect stdout to this file")

	config              *Config
	client              *bigtable.Client
	table               tableLike
	adminClient         *bigtable.AdminClient
	instanceAdminClient *bigtable.InstanceAdminClient

	version      = "<unknown version>"
	revision     = "<unknown revision>"
	revisionDate = "<unknown revision date>"
	cliUserAgent = "cbt-cli-go/unknown"
)

type tableLike interface {
	ReadRows(ctx context.Context, arg bigtable.RowSet, f func(bigtable.Row) bool, opts ...bigtable.ReadOption) (err error)
}

func getCredentialOpts(opts []option.ClientOption) []option.ClientOption {
	if ts := config.TokenSource; ts != nil {
		opts = append(opts, option.WithTokenSource(ts))
	}
	if tlsCreds := config.TLSCreds; tlsCreds != nil {
		opts = append(opts, option.WithGRPCDialOption(grpc.WithTransportCredentials(tlsCreds)))
	}
	return opts
}

func getClient(clientConf bigtable.ClientConfig) *bigtable.Client {
	if client == nil {
		var opts []option.ClientOption
		if ep := config.DataEndpoint; ep != "" {
			opts = append(opts, option.WithEndpoint(ep))
		}
		opts = append(opts, option.WithUserAgent(cliUserAgent))
		opts = getCredentialOpts(opts)
		var err error
		client, err = bigtable.NewClientWithConfig(context.Background(), config.Project, config.Instance, clientConf, opts...)
		if err != nil {
			log.Fatalf("Making bigtable.Client: %v", err)
		}
	}
	return client
}

func getTable(clientConf bigtable.ClientConfig, tableName string) tableLike {
	if table != nil {
		return table
	}
	table = getClient(clientConf).Open(tableName)
	return table
}

func getAdminClient() *bigtable.AdminClient {
	if adminClient == nil {
		var opts []option.ClientOption
		if ep := config.AdminEndpoint; ep != "" {
			opts = append(opts, option.WithEndpoint(ep))
		}
		opts = append(opts, option.WithUserAgent(cliUserAgent))
		opts = getCredentialOpts(opts)
		var err error
		adminClient, err = bigtable.NewAdminClient(context.Background(), config.Project, config.Instance, opts...)
		if err != nil {
			log.Fatalf("Making bigtable.AdminClient: %v", err)
		}
	}
	return adminClient
}

func getInstanceAdminClient() *bigtable.InstanceAdminClient {
	if instanceAdminClient == nil {
		var opts []option.ClientOption
		if ep := config.AdminEndpoint; ep != "" {
			opts = append(opts, option.WithEndpoint(ep))
		}
		opts = getCredentialOpts(opts)
		var err error
		instanceAdminClient, err = bigtable.NewInstanceAdminClient(context.Background(), config.Project, opts...)
		if err != nil {
			log.Fatalf("Making bigtable.InstanceAdminClient: %v", err)
		}
	}
	return instanceAdminClient
}

func main() {
	var err error
	config, err = Load()
	if err != nil {
		log.Fatal(err)
	}
	config.RegisterFlags()

	flag.Usage = func() { usage(os.Stderr) }
	flag.Parse()
	if flag.NArg() == 0 {
		usage(os.Stderr)
		os.Exit(1)
	}

	if *oFlag != "" {
		f, err := os.Create(*oFlag)
		if err != nil {
			log.Fatal(err)
		}
		defer func() {
			if err := f.Close(); err != nil {
				log.Fatal(err)
			}
		}()
		os.Stdout = f
	}

	doMain(config, flag.Args())
}

func doMain(config *Config, args []string) {
	if config.UserAgent != "" {
		cliUserAgent = config.UserAgent
	}

	var ctx context.Context
	if config.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), config.Timeout)
		defer cancel()
	} else {
		ctx = context.Background()
	}

	if config.AuthToken != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "x-goog-iam-authorization-token", config.AuthToken)
	}

	for _, cmd := range commands {
		if cmd.Name == args[0] {
			if err := config.CheckFlags(cmd.Required); err != nil {
				log.Fatal(err)
			}
			cmd.do(ctx, args[1:]...)
			return
		}
	}
	log.Fatalf("Unknown command %q", args[0])
}

func usage(w io.Writer) {
	fmt.Fprintf(w, "Usage: %s [flags] <command> ...\n", os.Args[0])
	flag.CommandLine.SetOutput(w)
	flag.CommandLine.PrintDefaults()
	fmt.Fprintf(w, "\n%s", cmdSummary)
}

var cmdSummary string // generated in init, below

func init() {
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 10, 8, 4, '\t', 0)
	for _, cmd := range commands {
		fmt.Fprintf(tw, "cbt %s\t%s\n", cmd.Name, cmd.Desc)
	}
	tw.Flush()
	buf.WriteString(configHelp)
	buf.WriteString("\ncbt " + version + " " + revision + " " + revisionDate + "\n")
	cmdSummary = buf.String()
}

const configHelp = `
Alpha features are not currently available to most Cloud Bigtable customers. Alpha
features might be changed in backward-incompatible ways and are not recommended
for production use. They are not subject to any SLA or deprecation policy.

Syntax rules for the Bash shell apply to the ` + "`cbt`" + ` tool. This means, for example,
that you must put quotes around values that contain spaces or operators. It also means that
if a value is arbitrary bytes, you need to prefix it with a dollar sign and use single quotes.

Example:

cbt -project my-project -instance my-instance lookup my-table $'\224\257\312W\365:\205d\333\2471\315\'


For convenience, you can add values for the -project, -instance, -creds, -admin-endpoint and -data-endpoint
options to your ~/.cbtrc file in the following format:


    project = my-project-123
    instance = my-instance
    creds = path-to-account-key.json
    admin-endpoint = hostname:port
    data-endpoint = hostname:port
    auth-token = AJAvW039NO1nDcijk_J6_rFXG_...
    timeout = 30s

All values are optional and can be overridden at the command prompt.
`

const docIntroTemplate = `The ` + "`cbt`" + ` tool is a command-line tool that allows you to interact with Cloud Bigtable.
See the [cbt overview](https://cloud.google.com/bigtable/docs/cbt-overview) to learn how to install the ` + "`cbt`" + ` tool.

Usage:

	cbt [-<option> <option-argument>] <command> <required-argument> [optional-argument]


The commands are:
{{range .Commands}}
    {{printf "%-25s %s" .Name .Desc}}{{end}}

The options are:
{{range .Flags}}
    -{{.Name}} string
        {{.Usage}}{{end}}

Example:  cbt -instance=my-instance ls

Use "cbt help \<command>" for more information about a command.

{{.ConfigHelp}}
`

var commands = []struct {
	Name, Desc string
	do         func(context.Context, ...string)
	Usage      string
	Required   RequiredFlags
}{
	{
		Name:     "count",
		Desc:     "Count rows in a table",
		do:       doCount,
		Usage:    "cbt count <table-id>",
		Required: ProjectAndInstanceRequired,
	},
	{
		Name: "createinstance",
		Desc: "Create an instance with an initial cluster",
		do:   doCreateInstance,
		Usage: "cbt createinstance <instance-id> <display-name> <cluster-id> <zone> <num-nodes> <storage-type>\n" +
			"  instance-id      Permanent, unique ID for the instance\n" +
			"  display-name     Description of the instance\n" +
			"  cluster-id       Permanent, unique ID for the cluster in the instance\n" +
			"  zone             The zone in which to create the cluster\n" +
			"  num-nodes        The number of nodes to create\n" +
			"  storage-type     SSD or HDD\n\n" +
			"    Example: cbt createinstance my-instance \"My instance\" my-instance-c1 us-central1-b 3 SSD",
		Required: ProjectRequired,
	},
	{
		Name: "createcluster",
		Desc: "Create a cluster in the configured instance ",
		do:   doCreateCluster,
		Usage: "cbt createcluster <cluster-id> <zone> <num-nodes> <storage-type>\n" +
			"  cluster-id       Permanent, unique ID for the cluster in the instance\n" +
			"  zone             The zone in which to create the cluster\n" +
			"  num-nodes        The number of nodes to create\n" +
			"  storage-type     SSD or HDD\n\n" +
			"    Example: cbt createcluster my-instance-c2 europe-west1-b 3 SSD",
		Required: ProjectAndInstanceRequired,
	},
	{
		Name: "createfamily",
		Desc: "Create a column family",
		do:   doCreateFamily,
		Usage: "cbt createfamily <table-id> <family>\n\n" +
			"    Example: cbt createfamily mobile-time-series stats_summary",
		Required: ProjectAndInstanceRequired,
	},
	{
		Name: "createtable",
		Desc: "Create a table",
		do:   doCreateTable,
		Usage: "cbt createtable <table-id> [families=<family>:gcpolicy=<gcpolicy-expression>,...]\n" +
			"   [splits=<split-row-key-1>,<split-row-key-2>,...]\n" +
			"  families     Column families and their associated garbage collection (gc) policies.\n" +
			"               Put gc policies in quotes when they include shell operators && and ||. For gcpolicy,\n" +
			"               see \"setgcpolicy\".\n" +
			"  splits       Row key(s) where the table should initially be split\n\n" +
			"    Example: cbt createtable mobile-time-series \"families=stats_summary:maxage=10d||maxversions=1,stats_detail:maxage=10d||maxversions=1\" splits=tablet,phone",
		Required: ProjectAndInstanceRequired,
	},
	{
		Name: "updatecluster",
		Desc: "Update a cluster in the configured instance",
		do:   doUpdateCluster,
		Usage: "cbt updatecluster <cluster-id> [num-nodes=<num-nodes>]\n" +
			"  cluster-id    Permanent, unique ID for the cluster in the instance\n" +
			"  num-nodes     The new number of nodes\n\n" +
			"    Example: cbt updatecluster my-instance-c1 num-nodes=5",
		Required: ProjectAndInstanceRequired,
	},
	{
		Name: "deleteinstance",
		Desc: "Delete an instance",
		do:   doDeleteInstance,
		Usage: "cbt deleteinstance <instance-id>\n\n" +
			"    Example: cbt deleteinstance my-instance",
		Required: ProjectRequired,
	},
	{
		Name: "deletecluster",
		Desc: "Delete a cluster from the configured instance ",
		do:   doDeleteCluster,
		Usage: "cbt deletecluster <cluster-id>\n\n" +
			"    Example: cbt deletecluster my-instance-c2",
		Required: ProjectAndInstanceRequired,
	},
	{
		Name: "deletecolumn",
		Desc: "Delete all cells in a column",
		do:   doDeleteColumn,
		Usage: "cbt deletecolumn <table-id> <row-key> <family> <column> [app-profile=<app-profile-id>]\n" +
			"  app-profile=<app-profile-id>        The app profile ID to use for the request\n\n" +
			"    Example: cbt deletecolumn mobile-time-series phone#4c410523#20190501 stats_summary os_name",
		Required: ProjectAndInstanceRequired,
	},
	{
		Name: "deletefamily",
		Desc: "Delete a column family",
		do:   doDeleteFamily,
		Usage: "cbt deletefamily <table-id> <family>\n\n" +
			"    Example: cbt deletefamily mobile-time-series stats_summary",
		Required: ProjectAndInstanceRequired,
	},
	{
		Name: "deleterow",
		Desc: "Delete a row",
		do:   doDeleteRow,
		Usage: "cbt deleterow <table-id> <row-key> [app-profile=<app-profile-id>]\n" +
			"  app-profile=<app-profile-id>        The app profile ID to use for the request\n\n" +
			"    Example: cbt deleterow mobile-time-series phone#4c410523#20190501",
		Required: ProjectAndInstanceRequired,
	},
	{
		Name: "deleteallrows",
		Desc: "Delete all rows",
		do:   doDeleteAllRows,
		Usage: "cbt deleteallrows <table-id>\n\n" +
			"    Example: cbt deleteallrows  mobile-time-series",
		Required: ProjectAndInstanceRequired,
	},
	{
		Name: "deletetable",
		Desc: "Delete a table",
		do:   doDeleteTable,
		Usage: "cbt deletetable <table-id>\n\n" +
			"    Example: cbt deletetable mobile-time-series",
		Required: ProjectAndInstanceRequired,
	},
	{
		Name:     "doc",
		Desc:     "Print godoc-suitable documentation for cbt",
		do:       doDoc,
		Usage:    "cbt doc",
		Required: NoneRequired,
	},
	{
		Name: "help",
		Desc: "Print help text",
		do:   doHelp,
		Usage: "cbt help <command>\n\n" +
			"    Example: cbt help createtable",
		Required: NoneRequired,
	},
	{
		Name: "import",
		Desc: "Batch write many rows based on the input file",
		do:   doImport,
		Usage: "cbt import <table-id> <input-file> [app-profile=<app-profile-id>] [column-family=<family-name>] [batch-size=<500>] [workers=<1>]\n" +
			"  app-profile=<app-profile-id>          The app profile ID to use for the request\n" +
			"  column-family=<family-name>           The column family label to use\n" +
			"  batch-size=<500>                      The max number of rows per batch write request\n" +
			"  workers=<1>                           The number of worker threads\n\n" +
			"  Import data from a CSV file into an existing Cloud Bigtable table that already has the column families your data requires.\n\n" +
			"  The CSV file can support two rows of headers:\n" +
			"      - (Optional) column families\n" +
			"      - Column qualifiers\n" +
			"  Because the first column is reserved for row keys, leave it empty in the header rows.\n" +
			"  In the column family header, provide each column family once; it applies to the column it is in and every column to the right until another column family is found.\n" +
			"  Each row after the header rows should contain a row key in the first column, followed by the data cells for the row.\n" +
			"  See the example below. If you don't provide a column family header row, the column header is your first row and your import command must include the `column-family` flag to specify an existing column family. \n\n" +
			"    ,column-family-1,,column-family-2,      // Optional column family row (1st cell empty)\n" +
			"    ,column-1,column-2,column-3,column-4    // Column qualifiers row (1st cell empty)\n" +
			"    a,TRUE,,,FALSE                          // Rowkey 'a' followed by data\n" +
			"    b,,,TRUE,FALSE                          // Rowkey 'b' followed by data\n" +
			"    c,,TRUE,,TRUE                           // Rowkey 'c' followed by data\n\n" +
			"  Examples:\n" +
			"    cbt import csv-import-table data.csv\n" +
			"    cbt import csv-import-table data-no-families.csv app-profile=batch-write-profile column-family=my-family workers=5\n",
		Required: ProjectAndInstanceRequired,
	},
	{
		Name:     "listinstances",
		Desc:     "List instances in a project",
		do:       doListInstances,
		Usage:    "cbt listinstances",
		Required: ProjectRequired,
	},
	{
		Name:     "listclusters",
		Desc:     "List clusters in an instance",
		do:       doListClusters,
		Usage:    "cbt listclusters",
		Required: ProjectAndInstanceRequired,
	},
	{
		Name: "lookup",
		Desc: "Read from a single row",
		do:   doLookup,
		Usage: "cbt lookup <table-id> <row-key> [columns=<family>:<qualifier>,...] [cells-per-column=<n>] " +
			" [app-profile=<app profile id>]\n" +
			"  row-key                             String or raw bytes. Raw bytes must be enclosed in single quotes and have a dollar-sign prefix\n" +
			"  columns=<family>:<qualifier>,...    Read only these columns, comma-separated\n" +
			"  cells-per-column=<n>                Read only this number of cells per column\n" +
			"  app-profile=<app-profile-id>        The app profile ID to use for the request\n\n" +
			" Example: cbt lookup mobile-time-series phone#4c410523#20190501 columns=stats_summary:os_build,os_name cells-per-column=1\n" +
			" Example: cbt lookup mobile-time-series $'\\x41\\x42'",
		Required: ProjectAndInstanceRequired,
	},
	{
		Name: "ls",
		Desc: "List tables and column families",
		do:   doLS,
		Usage: "cbt ls                List tables\n" +
			"cbt ls <table-id>     List column families in a table\n\n" +
			"    Example: cbt ls mobile-time-series",
		Required: ProjectAndInstanceRequired,
	},
	{
		Name:     "mddoc",
		Desc:     "Print documentation for cbt in Markdown format",
		do:       doMDDoc,
		Usage:    "cbt mddoc",
		Required: NoneRequired,
	},
	{
		Name: "read",
		Desc: "Read rows",
		do:   doRead,
		Usage: "cbt read <table-id> [start=<row-key>] [end=<row-key>] [prefix=<row-key-prefix>]" +
			" [regex=<regex>] [columns=<family>:<qualifier>,...] [count=<n>] [cells-per-column=<n>]" +
			" [app-profile=<app-profile-id>]\n" +
			"  start=<row-key>                     Start reading at this row\n" +
			"  end=<row-row>                       Stop reading before this row\n" +
			"  prefix=<row-key-prefix>             Read rows with this prefix\n" +
			"  regex=<regex>                       Read rows with keys matching this regex\n" +
			"  columns=<family>:<qualifier>,...    Read only these columns, comma-separated\n" +
			"  count=<n>                           Read only this many rows\n" +
			"  cells-per-column=<n>                Read only this many cells per column\n" +
			"  app-profile=<app-profile-id>        The app profile ID to use for the request\n\n" +
			"    Examples: (see 'set' examples to create data to read)\n" +
			"      cbt read mobile-time-series prefix=phone columns=stats_summary:os_build,os_name count=10\n" +
			"      cbt read mobile-time-series start=phone#4c410523#20190501 end=phone#4c410523#20190601\n" +
			"      cbt read mobile-time-series regex=\"phone.*\" cells-per-column=1\n\n" +
			"   Note: Using a regex without also specifying start, end, prefix, or count results in a full\n" +
			"   table scan, which can be slow.\n",
		Required: ProjectAndInstanceRequired,
	},
	{
		Name: "set",
		Desc: "Set value of a cell (write)",
		do:   doSet,
		Usage: "cbt set <table-id> <row-key> [app-profile=<app-profile-id>] <family>:<column>=<val>[@<timestamp>] ...\n" +
			"  app-profile=<app profile id>          The app profile ID to use for the request\n" +
			"  <family>:<column>=<val>[@<timestamp>] may be repeated to set multiple cells.\n\n" +
			"    timestamp is an optional integer. \n" +
			"    If the timestamp cannot be parsed, '@<timestamp>' will be interpreted as part of the value.\n" +
			"    For most uses, a timestamp is the number of microseconds since 1970-01-01 00:00:00 UTC.\n\n" +
			"    Examples:\n" +
			"      cbt set mobile-time-series phone#4c410523#20190501 stats_summary:connected_cell=1@12345 stats_summary:connected_cell=0@1570041766\n" +
			"      cbt set mobile-time-series phone#4c410523#20190501 stats_summary:os_build=PQ2A.190405.003 stats_summary:os_name=android",
		Required: ProjectAndInstanceRequired,
	},
	{
		Name: "setgcpolicy",
		Desc: "Set the garbage-collection policy (age, versions) for a column family",
		do:   doSetGCPolicy,
		Usage: "cbt setgcpolicy <table> <family> ((maxage=<d> | maxversions=<n>) [(and|or) (maxage=<d> | maxversions=<n>),...] | never)\n" +
			"  maxage=<d>         Maximum timestamp age to preserve. Acceptable units: ms, s, m, h, d\n" +
			"  maxversions=<n>    Maximum number of versions to preserve\n" +
			"  Put garbage collection policies in quotes when they include shell operators && and ||.\n\n" +
			"    Examples:\n" +
			"      cbt setgcpolicy mobile-time-series stats_detail maxage=10d\n" +
			"      cbt setgcpolicy mobile-time-series stats_summary maxage=10d or maxversions=1\n",
		Required: ProjectAndInstanceRequired,
	},
	{
		Name:     "waitforreplication",
		Desc:     "Block until all the completed writes have been replicated to all the clusters",
		do:       doWaitForReplicaiton,
		Usage:    "cbt waitforreplication <table-id>\n",
		Required: ProjectAndInstanceRequired,
	},
	{
		Name: "createtablefromsnapshot",
		Desc: "Create a table from a snapshot (snapshots alpha)",
		do:   doCreateTableFromSnapshot,
		Usage: "cbt createtablefromsnapshot <table> <cluster> <snapshot>\n" +
			"  table        The name of the table to create\n" +
			"  cluster      The cluster where the snapshot is located\n" +
			"  snapshot     The snapshot to restore\n",
		Required: ProjectAndInstanceRequired,
	},
	{
		Name: "createsnapshot",
		Desc: "Create a snapshot from a source table (snapshots alpha)",
		do:   doSnapshotTable,
		Usage: "cbt createsnapshot <cluster> <snapshot> <table> [ttl=<d>]\n" +
			`  [ttl=<d>]        Lifespan of the snapshot (e.g. "1h", "4d")`,
		Required: ProjectAndInstanceRequired,
	},
	{
		Name:     "listsnapshots",
		Desc:     "List snapshots in a cluster (snapshots alpha)",
		do:       doListSnapshots,
		Usage:    "cbt listsnapshots [<cluster>]",
		Required: ProjectAndInstanceRequired,
	},
	{
		Name:     "getsnapshot",
		Desc:     "Get snapshot info (snapshots alpha)",
		do:       doGetSnapshot,
		Usage:    "cbt getsnapshot <cluster> <snapshot>",
		Required: ProjectAndInstanceRequired,
	},
	{
		Name:     "deletesnapshot",
		Desc:     "Delete snapshot in a cluster (snapshots alpha)",
		do:       doDeleteSnapshot,
		Usage:    "cbt deletesnapshot <cluster> <snapshot>",
		Required: ProjectAndInstanceRequired,
	},
	{
		Name:     "version",
		Desc:     "Print the current cbt version",
		do:       doVersion,
		Usage:    "cbt version",
		Required: NoneRequired,
	},
	{
		Name: "createappprofile",
		Desc: "Create app profile for an instance",
		do:   doCreateAppProfile,
		Usage: "cbt createappprofile <instance-id> <app-profile-id> <description> " +
			"(route-any | [ route-to=<cluster-id> : transactional-writes]) [-force] \n" +
			"  force:  Optional flag to override any warnings causing the command to fail\n\n" +
			"    Examples:\n" +
			"      cbt createappprofile my-instance multi-cluster \"Routes to nearest available cluster\" route-any\n" +
			"      cbt createappprofile my-instance single-cluster \"Europe routing\" route-to=my-instance-c2",
		Required: ProjectAndInstanceRequired,
	},
	{
		Name:     "getappprofile",
		Desc:     "Read app profile for an instance",
		do:       doGetAppProfile,
		Usage:    "cbt getappprofile <instance-id> <profile-id>",
		Required: ProjectAndInstanceRequired,
	},
	{
		Name:     "listappprofile",
		Desc:     "Lists app profile for an instance",
		do:       doListAppProfiles,
		Usage:    "cbt listappprofile <instance-id> ",
		Required: ProjectAndInstanceRequired,
	},
	{
		Name: "updateappprofile",
		Desc: "Update app profile for an instance",
		do:   doUpdateAppProfile,
		Usage: "cbt updateappprofile  <instance-id> <profile-id> <description>" +
			"(route-any | [ route-to=<cluster-id> : transactional-writes]) [-force] \n" +
			"  force:  Optional flag to override any warnings causing the command to fail\n\n" +
			"    Example: cbt updateappprofile my-instance multi-cluster \"Use this one.\" route-any",
		Required: ProjectAndInstanceRequired,
	},
	{
		Name: "deleteappprofile",
		Desc: "Delete app profile for an instance",
		do:   doDeleteAppProfile,
		Usage: "cbt deleteappprofile <instance-id> <profile-id>\n\n" +
			"    Example: cbt deleteappprofile my-instance single-cluster",
		Required: ProjectAndInstanceRequired,
	},
}

func doCount(ctx context.Context, args ...string) {
	if len(args) != 1 {
		log.Fatal("usage: cbt count <table>")
	}
	tbl := getTable(bigtable.ClientConfig{}, args[0])

	n := 0
	err := tbl.ReadRows(ctx, bigtable.InfiniteRange(""), func(_ bigtable.Row) bool {
		n++
		return true
	}, bigtable.RowFilter(bigtable.StripValueFilter()))
	if err != nil {
		log.Fatalf("Reading rows: %v", err)
	}
	fmt.Println(n)
}

func doCreateTable(ctx context.Context, args ...string) {
	if len(args) < 1 {
		log.Fatal("usage: cbt createtable <table> [families=family[:gcpolicy],...] [splits=split,...]")
	}

	tblConf := bigtable.TableConf{TableID: args[0]}
	parsed, err := parseArgs(args[1:], []string{"families", "splits"})
	if err != nil {
		log.Fatal(err)
	}
	for key, val := range parsed {
		chunks, err := csv.NewReader(strings.NewReader(val)).Read()
		if err != nil {
			log.Fatalf("Invalid %s arg format: %v", key, err)
		}
		switch key {
		case "families":
			tblConf.Families = make(map[string]bigtable.GCPolicy)
			for _, family := range chunks {
				famPolicy := strings.Split(family, ":")
				var gcPolicy bigtable.GCPolicy
				if len(famPolicy) < 2 {
					gcPolicy = bigtable.MaxVersionsPolicy(1)
					log.Printf("Using default GC Policy of %v for family %v", gcPolicy, family)
				} else {
					gcPolicy, err = parseGCPolicy(famPolicy[1])
					if err != nil {
						log.Fatal(err)
					}
				}
				tblConf.Families[famPolicy[0]] = gcPolicy
			}
		case "splits":
			tblConf.SplitKeys = chunks
		}
	}

	if err := getAdminClient().CreateTableFromConf(ctx, &tblConf); err != nil {
		log.Fatalf("Creating table: %v", err)
	}
}

func doCreateFamily(ctx context.Context, args ...string) {
	if len(args) != 2 {
		log.Fatal("usage: cbt createfamily <table> <family>")
	}
	err := getAdminClient().CreateColumnFamily(ctx, args[0], args[1])
	if err != nil {
		log.Fatalf("Creating column family: %v", err)
	}
}

func doCreateInstance(ctx context.Context, args ...string) {
	if len(args) < 6 {
		log.Fatal("cbt createinstance <instance-id> <display-name> <cluster-id> <zone> <num-nodes> <storage type>")
	}

	numNodes, err := strconv.ParseInt(args[4], 0, 32)
	if err != nil {
		log.Fatalf("Bad num-nodes %q: %v", args[4], err)
	}

	sType, err := parseStorageType(args[5])
	if err != nil {
		log.Fatal(err)
	}

	ic := bigtable.InstanceWithClustersConfig{
		InstanceID:  args[0],
		DisplayName: args[1],
		Clusters: []bigtable.ClusterConfig{{
			ClusterID:   args[2],
			Zone:        args[3],
			NumNodes:    int32(numNodes),
			StorageType: sType,
		}},
	}
	err = getInstanceAdminClient().CreateInstanceWithClusters(ctx, &ic)
	if err != nil {
		log.Fatalf("Creating instance: %v", err)
	}
}

func doCreateCluster(ctx context.Context, args ...string) {
	if len(args) < 4 {
		log.Fatal("usage: cbt createcluster <cluster-id> <zone> <num-nodes> <storage type>")
	}

	numNodes, err := strconv.ParseInt(args[2], 0, 32)
	if err != nil {
		log.Fatalf("Bad num_nodes %q: %v", args[2], err)
	}

	sType, err := parseStorageType(args[3])
	if err != nil {
		log.Fatal(err)
	}

	cc := bigtable.ClusterConfig{
		InstanceID:  config.Instance,
		ClusterID:   args[0],
		Zone:        args[1],
		NumNodes:    int32(numNodes),
		StorageType: sType,
	}
	err = getInstanceAdminClient().CreateCluster(ctx, &cc)
	if err != nil {
		log.Fatalf("Creating cluster: %v", err)
	}
}

func doUpdateCluster(ctx context.Context, args ...string) {
	if len(args) < 2 {
		log.Fatal("cbt updatecluster <cluster-id> [num-nodes=num-nodes]")
	}

	numNodes := int64(0)
	parsed, err := parseArgs(args[1:], []string{"num-nodes"})
	if err != nil {
		log.Fatal(err)
	}
	if val, ok := parsed["num-nodes"]; ok {
		numNodes, err = strconv.ParseInt(val, 0, 32)
		if err != nil {
			log.Fatalf("Bad num-nodes %q: %v", val, err)
		}
	}
	if numNodes > 0 {
		err = getInstanceAdminClient().UpdateCluster(ctx, config.Instance, args[0], int32(numNodes))
		if err != nil {
			log.Fatalf("Updating cluster: %v", err)
		}
	} else {
		log.Fatal("Updating cluster: nothing to update")
	}
}

func doDeleteInstance(ctx context.Context, args ...string) {
	if len(args) != 1 {
		log.Fatal("usage: cbt deleteinstance <instance>")
	}
	err := getInstanceAdminClient().DeleteInstance(ctx, args[0])
	if err != nil {
		log.Fatalf("Deleting instance: %v", err)
	}
}

func doDeleteCluster(ctx context.Context, args ...string) {
	if len(args) != 1 {
		log.Fatal("usage: cbt deletecluster <cluster>")
	}
	err := getInstanceAdminClient().DeleteCluster(ctx, config.Instance, args[0])
	if err != nil {
		log.Fatalf("Deleting cluster: %v", err)
	}
}

func doDeleteColumn(ctx context.Context, args ...string) {
	usage := "usage: cbt deletecolumn <table> <row> <family> <column> [app-profile=<app profile id>]"
	if len(args) != 4 && len(args) != 5 {
		log.Fatal(usage)
	}
	var appProfile string
	if len(args) == 5 {
		if !strings.HasPrefix(args[4], "app-profile=") {
			log.Fatal(usage)
		}
		appProfile = strings.Split(args[4], "=")[1]
	}
	tbl := getClient(bigtable.ClientConfig{AppProfile: appProfile}).Open(args[0])
	mut := bigtable.NewMutation()
	mut.DeleteCellsInColumn(args[2], args[3])
	if err := tbl.Apply(ctx, args[1], mut); err != nil {
		log.Fatalf("Deleting cells in column: %v", err)
	}
}

func doDeleteFamily(ctx context.Context, args ...string) {
	if len(args) != 2 {
		log.Fatal("usage: cbt deletefamily <table> <family>")
	}
	err := getAdminClient().DeleteColumnFamily(ctx, args[0], args[1])
	if err != nil {
		log.Fatalf("Deleting column family: %v", err)
	}
}

func doDeleteRow(ctx context.Context, args ...string) {
	usage := "usage: cbt deleterow <table> <row> [app-profile=<app profile id>]"
	if len(args) != 2 && len(args) != 3 {
		log.Fatal(usage)
	}
	var appProfile string
	if len(args) == 3 {
		if !strings.HasPrefix(args[2], "app-profile=") {
			log.Fatal(usage)
		}
		appProfile = strings.Split(args[2], "=")[1]
	}
	tbl := getClient(bigtable.ClientConfig{AppProfile: appProfile}).Open(args[0])
	mut := bigtable.NewMutation()
	mut.DeleteRow()
	if err := tbl.Apply(ctx, args[1], mut); err != nil {
		log.Fatalf("Deleting row: %v", err)
	}
}

func doDeleteAllRows(ctx context.Context, args ...string) {
	if len(args) != 1 {
		log.Fatalf("Can't do `cbt deleteallrows %s`", args)
	}
	err := getAdminClient().DropAllRows(ctx, args[0])
	if err != nil {
		log.Fatalf("Deleting all rows: %v", err)
	}
}

func doDeleteTable(ctx context.Context, args ...string) {
	if len(args) != 1 {
		log.Fatalf("Can't do `cbt deletetable %s`", args)
	}
	err := getAdminClient().DeleteTable(ctx, args[0])
	if err != nil {
		log.Fatalf("Deleting table: %v", err)
	}
}

// to break circular dependencies
var (
	doDocFn   func(ctx context.Context, args ...string)
	doHelpFn  func(ctx context.Context, args ...string)
	doMDDocFn func(ctx context.Context, args ...string)
)

func init() {
	doDocFn = doDocReal
	doHelpFn = doHelpReal
	doMDDocFn = doMDDocReal
}

func doDoc(ctx context.Context, args ...string)   { doDocFn(ctx, args...) }
func doHelp(ctx context.Context, args ...string)  { doHelpFn(ctx, args...) }
func doMDDoc(ctx context.Context, args ...string) { doMDDocFn(ctx, args...) }

func docFlags() []*flag.Flag {
	// Only include specific flags, in a specific order.
	var flags []*flag.Flag
	for _, name := range []string{"project", "instance", "creds", "timeout"} {
		f := flag.Lookup(name)
		if f == nil {
			log.Fatalf("Flag not linked: -%s", name)
		}
		flags = append(flags, f)
	}
	return flags
}

func doDocReal(ctx context.Context, args ...string) {
	data := map[string]interface{}{
		"Commands":   commands,
		"Flags":      docFlags(),
		"ConfigHelp": configHelp,
	}
	var buf bytes.Buffer
	if err := docTemplate.Execute(&buf, data); err != nil {
		log.Fatalf("Bad doc template: %v", err)
	}
	out, err := format.Source(buf.Bytes())
	if err != nil {
		log.Fatalf("Bad doc output: %v", err)
	}
	os.Stdout.Write(out)
}

func indentLines(s, ind string) string {
	ss := strings.Split(s, "\n")
	for i, p := range ss {
		ss[i] = ind + p
	}
	return strings.Join(ss, "\n")
}

var docTemplate = template.Must(template.New("doc").Funcs(template.FuncMap{
	"indent": indentLines,
}).
	Parse(`
// Copyright 2016 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// DO NOT EDIT. THIS IS AUTOMATICALLY GENERATED.
// Run "go generate" to regenerate.
//go:generate go run cbt.go gcpolicy.go -o cbtdoc.go doc

/*
` + docIntroTemplate + `

{{range .Commands}}
{{.Desc}}

Usage:
{{indent .Usage "\t"}}



{{end}}
*/
package main
`))

func doHelpReal(ctx context.Context, args ...string) {
	if len(args) == 0 {
		usage(os.Stdout)
		return
	}
	for _, cmd := range commands {
		if cmd.Name == args[0] {
			fmt.Println(cmd.Usage)
			return
		}
	}
	log.Fatalf("Don't know command %q", args[0])
}

func doListInstances(ctx context.Context, args ...string) {
	if len(args) != 0 {
		log.Fatalf("usage: cbt listinstances")
	}
	is, err := getInstanceAdminClient().Instances(ctx)
	if err != nil {
		log.Fatalf("Getting list of instances: %v", err)
	}
	tw := tabwriter.NewWriter(os.Stdout, 10, 8, 4, '\t', 0)
	fmt.Fprintf(tw, "Instance Name\tInfo\n")
	fmt.Fprintf(tw, "-------------\t----\n")
	for _, i := range is {
		fmt.Fprintf(tw, "%s\t%s\n", i.Name, i.DisplayName)
	}
	tw.Flush()
}

func doListClusters(ctx context.Context, args ...string) {
	if len(args) != 0 {
		log.Fatalf("usage: cbt listclusters")
	}
	cis, err := getInstanceAdminClient().Clusters(ctx, config.Instance)
	if err != nil {
		log.Fatalf("Getting list of clusters: %v", err)
	}
	tw := tabwriter.NewWriter(os.Stdout, 10, 8, 4, '\t', 0)
	fmt.Fprintf(tw, "Cluster Name\tZone\tState\n")
	fmt.Fprintf(tw, "------------\t----\t----\n")
	for _, ci := range cis {
		fmt.Fprintf(tw, "%s\t%s\t%s (%d serve nodes)\n", ci.Name, ci.Zone, ci.State, ci.ServeNodes)
	}
	tw.Flush()
}

func doLookup(ctx context.Context, args ...string) {
	if len(args) < 2 {
		log.Fatalf("usage: cbt lookup <table> <row> [columns=<family:qualifier>...] [cells-per-column=<n>] " +
			"[app-profile=<app profile id>]")
	}

	parsed, err := parseArgs(args[2:], []string{"columns", "cells-per-column", "app-profile"})
	if err != nil {
		log.Fatal(err)
	}
	var opts []bigtable.ReadOption
	var filters []bigtable.Filter
	if cellsPerColumn := parsed["cells-per-column"]; cellsPerColumn != "" {
		n, err := strconv.Atoi(cellsPerColumn)
		if err != nil {
			log.Fatalf("Bad number of cells per column %q: %v", cellsPerColumn, err)
		}
		filters = append(filters, bigtable.LatestNFilter(n))
	}
	if columns := parsed["columns"]; columns != "" {
		columnFilters, err := parseColumnsFilter(columns)
		if err != nil {
			log.Fatal(err)
		}
		filters = append(filters, columnFilters)
	}

	if len(filters) > 1 {
		opts = append(opts, bigtable.RowFilter(bigtable.ChainFilters(filters...)))
	} else if len(filters) == 1 {
		opts = append(opts, bigtable.RowFilter(filters[0]))
	}

	table, row := args[0], args[1]
	tbl := getClient(bigtable.ClientConfig{AppProfile: parsed["app-profile"]}).Open(table)
	r, err := tbl.ReadRow(ctx, row, opts...)
	if err != nil {
		log.Fatalf("Reading row: %v", err)
	}
	printRow(r)
}

func printRow(r bigtable.Row) {
	fmt.Println(strings.Repeat("-", 40))
	fmt.Println(r.Key())

	var fams []string
	for fam := range r {
		fams = append(fams, fam)
	}
	sort.Strings(fams)
	for _, fam := range fams {
		ris := r[fam]
		sort.Sort(byColumn(ris))
		for _, ri := range ris {
			ts := time.Unix(0, int64(ri.Timestamp)*1e3)
			fmt.Printf("  %-40s @ %s\n", ri.Column, ts.Format("2006/01/02-15:04:05.000000"))
			fmt.Printf("    %q\n", ri.Value)
		}
	}
}

type byColumn []bigtable.ReadItem

func (b byColumn) Len() int           { return len(b) }
func (b byColumn) Swap(i, j int)      { b[i], b[j] = b[j], b[i] }
func (b byColumn) Less(i, j int) bool { return b[i].Column < b[j].Column }

type byFamilyName []bigtable.FamilyInfo

func (b byFamilyName) Len() int           { return len(b) }
func (b byFamilyName) Swap(i, j int)      { b[i], b[j] = b[j], b[i] }
func (b byFamilyName) Less(i, j int) bool { return b[i].Name < b[j].Name }

func doLS(ctx context.Context, args ...string) {
	switch len(args) {
	default:
		log.Fatalf("Can't do `cbt ls %s`", args)
	case 0:
		tables, err := getAdminClient().Tables(ctx)
		if err != nil {
			log.Fatalf("Getting list of tables: %v", err)
		}
		sort.Strings(tables)
		for _, table := range tables {
			fmt.Println(table)
		}
	case 1:
		table := args[0]
		ti, err := getAdminClient().TableInfo(ctx, table)
		if err != nil {
			log.Fatalf("Getting table info: %v", err)
		}
		sort.Sort(byFamilyName(ti.FamilyInfos))
		tw := tabwriter.NewWriter(os.Stdout, 10, 8, 4, '\t', 0)
		fmt.Fprintf(tw, "Family Name\tGC Policy\n")
		fmt.Fprintf(tw, "-----------\t---------\n")
		for _, fam := range ti.FamilyInfos {
			fmt.Fprintf(tw, "%s\t%s\n", fam.Name, fam.GCPolicy)
		}
		tw.Flush()
	}
}

func doMDDocReal(ctx context.Context, args ...string) {
	data := map[string]interface{}{
		"Commands":   commands,
		"Flags":      docFlags(),
		"ConfigHelp": configHelp,
	}
	var buf bytes.Buffer
	if err := mddocTemplate.Execute(&buf, data); err != nil {
		log.Fatalf("Bad mddoc template: %v", err)
	}
	io.Copy(os.Stdout, &buf)
}

var mddocTemplate = template.Must(template.New("mddoc").Funcs(template.FuncMap{
	"indent": indentLines,
}).
	Parse(docIntroTemplate + `


{{range .Commands}}
## {{.Desc}}

{{indent .Usage "\t"}}



{{end}}
`))

func doRead(ctx context.Context, args ...string) {
	if len(args) < 1 {
		log.Fatalf("usage: cbt read <table> [args ...]")
	}

	parsed, err := parseArgs(args[1:], []string{
		"start", "end", "prefix", "columns", "count", "cells-per-column", "regex", "app-profile", "limit",
	})
	if err != nil {
		log.Fatal(err)
	}
	if _, ok := parsed["limit"]; ok {
		// Be nicer; we used to support this, but renamed it to "end".
		log.Fatal("Unknown arg key 'limit'; did you mean 'end'?")
	}
	if (parsed["start"] != "" || parsed["end"] != "") && parsed["prefix"] != "" {
		log.Fatal(`"start"/"end" may not be mixed with "prefix"`)
	}

	var rr bigtable.RowRange
	if start, end := parsed["start"], parsed["end"]; end != "" {
		rr = bigtable.NewRange(start, end)
	} else if start != "" {
		rr = bigtable.InfiniteRange(start)
	}
	if prefix := parsed["prefix"]; prefix != "" {
		rr = bigtable.PrefixRange(prefix)
	}

	var opts []bigtable.ReadOption
	if count := parsed["count"]; count != "" {
		n, err := strconv.ParseInt(count, 0, 64)
		if err != nil {
			log.Fatalf("Bad count %q: %v", count, err)
		}
		opts = append(opts, bigtable.LimitRows(n))
	}

	var filters []bigtable.Filter
	if cellsPerColumn := parsed["cells-per-column"]; cellsPerColumn != "" {
		n, err := strconv.Atoi(cellsPerColumn)
		if err != nil {
			log.Fatalf("Bad number of cells per column %q: %v", cellsPerColumn, err)
		}
		filters = append(filters, bigtable.LatestNFilter(n))
	}
	if regex := parsed["regex"]; regex != "" {
		filters = append(filters, bigtable.RowKeyFilter(regex))
	}
	if columns := parsed["columns"]; columns != "" {
		columnFilters, err := parseColumnsFilter(columns)
		if err != nil {
			log.Fatal(err)
		}
		filters = append(filters, columnFilters)
	}

	if len(filters) > 1 {
		opts = append(opts, bigtable.RowFilter(bigtable.ChainFilters(filters...)))
	} else if len(filters) == 1 {
		opts = append(opts, bigtable.RowFilter(filters[0]))
	}

	// TODO(dsymonds): Support filters.
	tbl := getClient(bigtable.ClientConfig{AppProfile: parsed["app-profile"]}).Open(args[0])
	err = tbl.ReadRows(ctx, rr, func(r bigtable.Row) bool {
		printRow(r)
		return true
	}, opts...)
	if err != nil {
		log.Fatalf("Reading rows: %v", err)
	}
}

var setArg = regexp.MustCompile(`([^:]+):([^=]*)=(.*)`)

func doSet(ctx context.Context, args ...string) {
	if len(args) < 3 {
		log.Fatalf("usage: cbt set <table> <row> [app-profile=<app profile id>] family:[column]=val[@ts] ...")
	}
	var appProfile string
	row := args[1]
	mut := bigtable.NewMutation()
	for _, arg := range args[2:] {
		if strings.HasPrefix(arg, "app-profile=") {
			appProfile = strings.Split(arg, "=")[1]
			continue
		}
		m := setArg.FindStringSubmatch(arg)
		if m == nil {
			log.Fatalf("Bad set arg %q", arg)
		}
		val := m[3]
		ts := bigtable.Now()
		if i := strings.LastIndex(val, "@"); i >= 0 {
			// Try parsing a timestamp.
			n, err := strconv.ParseInt(val[i+1:], 0, 64)
			if err == nil {
				val = val[:i]
				ts = bigtable.Timestamp(n)
			}
		}
		mut.Set(m[1], m[2], ts, []byte(val))
	}
	tbl := getClient(bigtable.ClientConfig{AppProfile: appProfile}).Open(args[0])
	if err := tbl.Apply(ctx, row, mut); err != nil {
		log.Fatalf("Applying mutation: %v", err)
	}
}

func doSetGCPolicy(ctx context.Context, args ...string) {
	if len(args) < 3 {
		log.Fatalf("usage: cbt setgcpolicy <table> <family> ((maxage=<d> | maxversions=<n>) [(and|or) (maxage=<d> | maxversions=<n>),...] | never)")
	}
	table := args[0]
	fam := args[1]
	pol, err := parseGCPolicy(strings.Join(args[2:], " "))
	if err != nil {
		log.Fatal(err)
	}
	if err := getAdminClient().SetGCPolicy(ctx, table, fam, pol); err != nil {
		log.Fatalf("Setting GC policy: %v", err)
	}
}

func doWaitForReplicaiton(ctx context.Context, args ...string) {
	if len(args) != 1 {
		log.Fatalf("usage: cbt waitforreplication <table>")
	}
	table := args[0]

	fmt.Printf("Waiting for all writes up to %s to be replicated.\n", time.Now().Format("2006/01/02-15:04:05"))
	if err := getAdminClient().WaitForReplication(ctx, table); err != nil {
		log.Fatalf("Waiting for replication: %v", err)
	}
}

func parseStorageType(storageTypeStr string) (bigtable.StorageType, error) {
	switch storageTypeStr {
	case "SSD":
		return bigtable.SSD, nil
	case "HDD":
		return bigtable.HDD, nil
	}
	return -1, fmt.Errorf("Invalid storage type: %v, must be SSD or HDD", storageTypeStr)
}

func doCreateTableFromSnapshot(ctx context.Context, args ...string) {
	if len(args) != 3 {
		log.Fatal("usage: cbt createtablefromsnapshot <table> <cluster> <snapshot>")
	}
	tableName := args[0]
	clusterName := args[1]
	snapshotName := args[2]
	err := getAdminClient().CreateTableFromSnapshot(ctx, tableName, clusterName, snapshotName)

	if err != nil {
		log.Fatalf("Creating table: %v", err)
	}
}

func doSnapshotTable(ctx context.Context, args ...string) {
	if len(args) != 3 && len(args) != 4 {
		log.Fatal("usage: cbt createsnapshot <cluster> <snapshot> <table> [ttl=<d>]")
	}
	clusterName := args[0]
	snapshotName := args[1]
	tableName := args[2]
	ttl := bigtable.DefaultSnapshotDuration

	parsed, err := parseArgs(args[3:], []string{"ttl"})
	if err != nil {
		log.Fatal(err)
	}
	if val, ok := parsed["ttl"]; ok {
		var err error
		ttl, err = parseDuration(val)
		if err != nil {
			log.Fatalf("Invalid snapshot ttl value %q: %v", val, err)
		}
	}

	err = getAdminClient().SnapshotTable(ctx, tableName, clusterName, snapshotName, ttl)
	if err != nil {
		log.Fatalf("Failed to create Snapshot: %v", err)
	}
}

func doListSnapshots(ctx context.Context, args ...string) {
	if len(args) != 0 && len(args) != 1 {
		log.Fatal("usage: cbt listsnapshots [<cluster>]")
	}

	var cluster string

	if len(args) == 0 {
		cluster = "-"
	} else {
		cluster = args[0]
	}

	it := getAdminClient().Snapshots(ctx, cluster)

	tw := tabwriter.NewWriter(os.Stdout, 10, 8, 4, '\t', 0)
	fmt.Fprintf(tw, "Snapshot\tSource Table\tCreated At\tExpires At\n")
	fmt.Fprintf(tw, "--------\t------------\t----------\t----------\n")
	timeLayout := "2006-01-02 15:04 MST"

	for {
		snapshot, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			log.Fatalf("Failed to fetch snapshots %v", err)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", snapshot.Name, snapshot.SourceTable, snapshot.CreateTime.Format(timeLayout), snapshot.DeleteTime.Format(timeLayout))
	}
	tw.Flush()
}

func doGetSnapshot(ctx context.Context, args ...string) {
	if len(args) != 2 {
		log.Fatalf("usage: cbt getsnapshot <cluster> <snapshot>")
	}
	clusterName := args[0]
	snapshotName := args[1]

	snapshot, err := getAdminClient().SnapshotInfo(ctx, clusterName, snapshotName)
	if err != nil {
		log.Fatalf("Failed to get snapshot: %v", err)
	}

	timeLayout := "2006-01-02 15:04 MST"

	fmt.Printf("Name: %s\n", snapshot.Name)
	fmt.Printf("Source table: %s\n", snapshot.SourceTable)
	fmt.Printf("Created at: %s\n", snapshot.CreateTime.Format(timeLayout))
	fmt.Printf("Expires at: %s\n", snapshot.DeleteTime.Format(timeLayout))
}

func doDeleteSnapshot(ctx context.Context, args ...string) {
	if len(args) != 2 {
		log.Fatal("usage: cbt deletesnapshot <cluster> <snapshot>")
	}
	cluster := args[0]
	snapshot := args[1]

	err := getAdminClient().DeleteSnapshot(ctx, cluster, snapshot)

	if err != nil {
		log.Fatalf("Failed to delete snapshot: %v", err)
	}
}

func doCreateAppProfile(ctx context.Context, args ...string) {
	if len(args) < 4 || len(args) > 6 {
		log.Fatal("usage: cbt createappprofile <instance-id> <profile-id> <description> " +
			" (route-any | [ route-to=<cluster-id> : transactional-writes]) [optional flag] \n" +
			"optional flags may be `force`")
	}

	routingPolicy, clusterID, err := parseProfileRoute(args[3])
	if err != nil {
		log.Fatalln("Exactly one of (route-any | [route-to : transactional-writes]) must be specified.")
	}

	config := bigtable.ProfileConf{
		RoutingPolicy: routingPolicy,
		InstanceID:    args[0],
		ProfileID:     args[1],
		Description:   args[2],
	}

	opFlags := []string{"force", "transactional-writes"}
	parseValues, err := parseArgs(args[4:], opFlags)
	if err != nil {
		log.Fatalf("optional flags can be specified as (force=<true>|transactional-writes=<true>) got %s ", args[4:])
	}

	for _, f := range opFlags {
		fv, err := parseProfileOpts(f, parseValues)
		if err != nil {
			log.Fatalf("optional flags can be specified as (force=<true>|transactional-writes=<true>) got %s ", args[4:])
		}

		switch f {
		case opFlags[0]:
			config.IgnoreWarnings = fv
		case opFlags[1]:
			config.AllowTransactionalWrites = fv
		default:

		}
	}

	if routingPolicy == bigtable.SingleClusterRouting {
		config.ClusterID = clusterID
	}

	profile, err := getInstanceAdminClient().CreateAppProfile(ctx, config)
	if err != nil {
		log.Fatalf("Failed to create app profile : %v", err)
	}

	fmt.Printf("Name: %s\n", profile.Name)
	fmt.Printf("RoutingPolicy: %v\n", profile.RoutingPolicy)
}

func doGetAppProfile(ctx context.Context, args ...string) {
	if len(args) != 2 {
		log.Fatalln("usage: cbt getappprofile <instance-id> <profile-id>")
	}

	instanceID := args[0]
	profileID := args[1]
	profile, err := getInstanceAdminClient().GetAppProfile(ctx, instanceID, profileID)
	if err != nil {
		log.Fatalf("Failed to get app profile : %v", err)
	}

	fmt.Printf("Name: %s\n", profile.Name)
	fmt.Printf("Etag: %s\n", profile.Etag)
	fmt.Printf("Description: %s\n", profile.Description)
	fmt.Printf("RoutingPolicy: %v\n", profile.RoutingPolicy)
}

func doListAppProfiles(ctx context.Context, args ...string) {
	if len(args) != 1 {
		log.Fatalln("usage: cbt listappprofile <instance-id>")
	}

	instance := args[0]

	it := getInstanceAdminClient().ListAppProfiles(ctx, instance)

	tw := tabwriter.NewWriter(os.Stdout, 10, 8, 4, '\t', 0)
	fmt.Fprintf(tw, "AppProfile\tProfile Description\tProfile Etag\tProfile Routing Policy\n")
	fmt.Fprintf(tw, "-----------\t--------------------\t------------\t----------------------\n")

	for {
		profile, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			log.Fatalf("Failed to fetch app profile %v", err)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", profile.Name, profile.Description, profile.Etag, profile.RoutingPolicy)
	}
	tw.Flush()
}

func doUpdateAppProfile(ctx context.Context, args ...string) {

	if len(args) < 4 {
		log.Fatal("usage: cbt updateappprofile  <instance-id> <profile-id> <description>" +
			" (route-any | [ route-to=<cluster-id> : transactional-writes]) [optional flag] \n" +
			"optional flags may be `force`")
	}

	routingPolicy, clusterID, err := parseProfileRoute(args[3])
	if err != nil {
		log.Fatalln("Exactly one of (route-any | [route-to : transactional-writes]) must be specified.")
	}
	InstanceID := args[0]
	ProfileID := args[1]
	config := bigtable.ProfileAttrsToUpdate{
		RoutingPolicy: routingPolicy,
		Description:   args[2],
	}
	opFlags := []string{"force", "transactional-writes"}
	parseValues, err := parseArgs(args[4:], opFlags)
	if err != nil {
		log.Fatalf("optional flags can be specified as (force=<true>|transactional-writes=<true>) got %s ", args[4:])
	}

	for _, f := range opFlags {
		fv, err := parseProfileOpts(f, parseValues)
		if err != nil {
			log.Fatalf("optional flags can be specified as (force=<true>|transactional-writes=<true>) got %s ", args[4:])
		}

		switch f {
		case opFlags[0]:
			config.IgnoreWarnings = fv
		case opFlags[1]:
			config.AllowTransactionalWrites = fv
		default:

		}
	}
	if routingPolicy == bigtable.SingleClusterRouting {
		config.ClusterID = clusterID
	}

	err = getInstanceAdminClient().UpdateAppProfile(ctx, InstanceID, ProfileID, config)
	if err != nil {
		log.Fatalf("Failed to update app profile : %v", err)
	}
}

func doDeleteAppProfile(ctx context.Context, args ...string) {
	if len(args) != 2 {
		log.Println("usage: cbt deleteappprofile <instance-id> <profile-id>")
	}

	err := getInstanceAdminClient().DeleteAppProfile(ctx, args[0], args[1])
	if err != nil {
		log.Fatalf("Failed to delete  app profile : %v", err)
	}
}

type importerArgs struct {
	appProfile string
	fam        string
	sz         int
	workers    int
}

type safeReader struct {
	mu sync.Mutex
	r  *csv.Reader
	t  int // total rows
}

func doImport(ctx context.Context, args ...string) {
	ia, err := parseImporterArgs(ctx, args)
	if err != nil {
		log.Fatalf("error parsing importer args: %s", err)
	}
	f, err := os.Open(args[1])
	if err != nil {
		log.Fatalf("couldn't open the csv file: %s", err)
	}

	tbl := getClient(bigtable.ClientConfig{AppProfile: ia.appProfile}).Open(args[0])
	r := csv.NewReader(f)
	importCSV(ctx, tbl, r, ia)
}

func parseImporterArgs(ctx context.Context, args []string) (importerArgs, error) {
	var err error
	ia := importerArgs{
		fam:     "",
		sz:      500,
		workers: 1,
	}
	if len(args) < 2 {
		return ia, fmt.Errorf("usage: cbt import <table-id> <input-file> [app-profile=<app-profile-id>] [column-family=<family-name>] [batch-size=<500>] [workers=<1>]")
	}
	for _, arg := range args[2:] {
		switch {
		case strings.HasPrefix(arg, "app-profile="):
			ia.appProfile = strings.Split(arg, "=")[1]
		case strings.HasPrefix(arg, "column-family="):
			ia.fam = strings.Split(arg, "=")[1]
			if ia.fam == "" {
				return ia, fmt.Errorf("column-family cannot be ''")
			}
		case strings.HasPrefix(arg, "batch-size="):
			ia.sz, err = strconv.Atoi(strings.Split(arg, "=")[1])
			if err != nil || ia.sz <= 0 || ia.sz >= 100000 {
				return ia, fmt.Errorf("batch-size must be > 0 and <= 100000")
			}
		case strings.HasPrefix(arg, "workers="):
			ia.workers, err = strconv.Atoi(strings.Split(arg, "=")[1])
			if err != nil || ia.workers <= 0 {
				return ia, fmt.Errorf("workers must be > 0, err:%s", err)
			}
		}
	}
	return ia, nil
}

func importCSV(ctx context.Context, tbl *bigtable.Table, r *csv.Reader, ia importerArgs) {
	fams, cols, err := parseCsvHeaders(r, ia.fam)
	if err != nil {
		log.Fatalf("error parsing headers: %s", err)
	}
	sr := safeReader{r: r}
	ts := bigtable.Now()

	var wg sync.WaitGroup
	wg.Add(ia.workers)
	for i := 0; i < ia.workers; i++ {
		go func(w int) {
			defer wg.Done()
			if e := sr.parseAndWrite(ctx, tbl, fams, cols, ts, ia.sz, w); e != nil {
				log.Fatalf("error: %s", e)
			}
		}(i)
	}
	wg.Wait()
	log.Printf("Done importing %d rows.\n", sr.t)
}

func parseCsvHeaders(r *csv.Reader, family string) ([]string, []string, error) {
	var err error
	var fams, cols []string
	if family == "" { // no column-family from flag, using first row
		fams, err = r.Read()
		if err != nil {
			return nil, nil, fmt.Errorf("family header reader error:%s", err)
		}
	}
	cols, err = r.Read() // column names are next row
	if err != nil {
		return nil, nil, fmt.Errorf("columns header reader error:%s", err)
	}
	if family != "" {
		fams = make([]string, len(cols))
		fams[1] = family
	}
	if len(fams) < 2 || len(cols) < 2 {
		return fams, cols, fmt.Errorf("at least 2 columns are required (rowkey + data)")
	}
	if fams[0] != "" || cols[0] != "" {
		return fams, cols, fmt.Errorf("the first column must be empty for column-family and column name rows")
	}
	if fams[1] == "" || cols[1] == "" {
		return fams, cols, fmt.Errorf("the second column (first data column) must have values for column family and column name rows if present")
	}
	for i := range cols { // fill any blank column families with previous
		if i > 0 && fams[i] == "" {
			fams[i] = fams[i-1]
		}
	}
	return fams, cols, nil
}

func batchWrite(ctx context.Context, tbl *bigtable.Table, rk []string, muts []*bigtable.Mutation, worker int) (int, error) {
	log.Printf("[%d] Writing batch:: size: %d, firstRowKey: %s, lastRowKey: %s\n", worker, len(rk), rk[0], rk[len(rk)-1])
	errors, err := tbl.ApplyBulk(ctx, rk, muts)
	if err != nil {
		return 0, fmt.Errorf("applying bulk mutations process error: %v", err)
	}
	if errors != nil {
		return 0, fmt.Errorf("applying bulk mutations had %d errors, first:%v", len(errors), errors[0])

	}
	return len(rk), nil
}

func (sr *safeReader) parseAndWrite(ctx context.Context, tbl *bigtable.Table, fams, cols []string, ts bigtable.Timestamp, max, worker int) error {
	var rowKey []string
	var muts []*bigtable.Mutation
	var c int
	for {
		sr.mu.Lock()
		for len(rowKey) < max {
			line, err := sr.r.Read()
			if err == io.EOF {
				break
			}
			if err != nil {
				log.Fatal(err)
			}
			mut := bigtable.NewMutation()
			empty := true
			for i, val := range line {
				if i > 0 && val != "" {
					mut.Set(fams[i], cols[i], ts, []byte(val))
					empty = false
				}
			}
			if empty {
				log.Printf("[%d] RowKey '%s' has no mutations, skipping", worker, line[0])
				continue
			}
			if line[0] == "" {
				log.Printf("[%d] RowKey not present, skipping line", worker)
				continue
			}
			rowKey = append(rowKey, line[0])
			muts = append(muts, mut)
		}
		if len(rowKey) > 0 {
			sr.mu.Unlock()
			n, err := batchWrite(ctx, tbl, rowKey, muts, worker)
			if err != nil {
				return err
			}
			c += n
			rowKey = rowKey[:0]
			muts = muts[:0]
			continue
		}
		sr.t += c
		sr.mu.Unlock()
		return nil
	}
}

// parseDuration parses a duration string.
// It is similar to Go's time.ParseDuration, except with a different set of supported units,
// and only simple formats supported.
func parseDuration(s string) (time.Duration, error) {
	// [0-9]+[a-z]+

	// Split [0-9]+ from [a-z]+.
	i := 0
	for ; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			break
		}
	}
	ds, u := s[:i], s[i:]
	if ds == "" || u == "" {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	// Parse them.
	d, err := strconv.ParseUint(ds, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %v", s, err)
	}
	unit, ok := unitMap[u]
	if !ok {
		return 0, fmt.Errorf("unknown unit %q in duration %q", u, s)
	}
	if d > uint64((1<<63-1)/unit) {
		// overflow
		return 0, fmt.Errorf("invalid duration %q overflows", s)
	}
	return time.Duration(d) * unit, nil
}

var unitMap = map[string]time.Duration{
	"ms": time.Millisecond,
	"s":  time.Second,
	"m":  time.Minute,
	"h":  time.Hour,
	"d":  24 * time.Hour,
}

func doVersion(ctx context.Context, args ...string) {
	fmt.Printf("%s %s %s\n", version, revision, revisionDate)
}

// parseArgs takes a slice of arguments of the form key=value and returns a map from
// key to value. It returns an error if an argument is malformed or a key is not in
// the valid slice.
func parseArgs(args []string, valid []string) (map[string]string, error) {
	parsed := make(map[string]string)
	for _, arg := range args {
		i := strings.Index(arg, "=")
		if i < 0 {
			return nil, fmt.Errorf("Bad arg %q", arg)
		}
		key, val := arg[:i], arg[i+1:]
		if !stringInSlice(key, valid) {
			return nil, fmt.Errorf("Unknown arg key %q", key)
		}
		parsed[key] = val
	}
	return parsed, nil
}

func stringInSlice(s string, list []string) bool {
	for _, e := range list {
		if s == e {
			return true
		}
	}
	return false
}

func parseColumnsFilter(columns string) (bigtable.Filter, error) {
	splitColumns := strings.FieldsFunc(columns, func(c rune) bool { return c == ',' })
	if len(splitColumns) == 1 {
		filter, err := columnFilter(splitColumns[0])
		if err != nil {
			return nil, err
		}
		return filter, nil
	}

	var columnFilters []bigtable.Filter
	for _, column := range splitColumns {
		filter, err := columnFilter(column)
		if err != nil {
			return nil, err
		}
		columnFilters = append(columnFilters, filter)
	}
	return bigtable.InterleaveFilters(columnFilters...), nil
}

func columnFilter(column string) (bigtable.Filter, error) {
	splitColumn := strings.Split(column, ":")
	if len(splitColumn) == 1 {
		return bigtable.ColumnFilter(splitColumn[0]), nil
	} else if len(splitColumn) == 2 {
		if strings.HasSuffix(column, ":") {
			return bigtable.FamilyFilter(splitColumn[0]), nil
		} else if strings.HasPrefix(column, ":") {
			return bigtable.ColumnFilter(splitColumn[1]), nil
		} else {
			familyFilter := bigtable.FamilyFilter(splitColumn[0])
			qualifierFilter := bigtable.ColumnFilter(splitColumn[1])
			return bigtable.ChainFilters(familyFilter, qualifierFilter), nil
		}
	} else {
		return nil, fmt.Errorf("Bad format for column %q", column)
	}
}

func parseProfileRoute(str string) (routingPolicy, clusterID string, err error) {

	route := strings.Split(str, "=")
	switch route[0] {
	case "route-any":
		if len(route) > 1 {
			err = fmt.Errorf("got %v", route)
			break
		}
		routingPolicy = bigtable.MultiClusterRouting

	case "route-to":
		if len(route) != 2 || route[1] == "" {
			err = fmt.Errorf("got %v", route)
			break
		}
		routingPolicy = bigtable.SingleClusterRouting
		clusterID = route[1]
	default:
		err = fmt.Errorf("got %v", route)
	}

	return
}

func parseProfileOpts(opt string, parsedArgs map[string]string) (bool, error) {

	if val, ok := parsedArgs[opt]; ok {
		status, err := strconv.ParseBool(val)
		if err != nil {
			return false, fmt.Errorf("expected %s = <true> got %s ", opt, val)
		}

		return status, nil
	}
	return false, nil
}
