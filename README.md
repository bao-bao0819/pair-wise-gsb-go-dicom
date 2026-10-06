<p align="center">
  <img src="https://suyashkumar.com/assets/img/magnetic-resonance.png" width="125px"/>
  <h3 align="center">dicom</h3>
  <p align="center">High Performance Golang DICOM Medical Image Parser<p>
  <p align="center"> 
    <a href="https://github.com/suyashkumar/dicom/actions"><img src="https://github.com/suyashkumar/dicom/workflows/build/badge.svg" /></a> 
    <a href="https://godoc.org/github.com/suyashkumar/dicom"><img src="https://godoc.org/github.com/suyashkumar/dicom?status.svg" alt="" /></a>
    <a href="https://goreportcard.com/report/github.com/suyashkumar/dicom"><img src="https://goreportcard.com/badge/github.com/suyashkumar/dicom" alt=""></a> 
  </p>
</p>

:eyes: __v1.0__ just released!

This is a library and command-line tool to read, write, and generally work with DICOM medical image files in native Go. The goal is to build a full-featured, high-performance, and readable DICOM parser for the Go community.

After a fair bit of work, I've just released v1.0 of this library which is essentially rewritten from the ground up to be more canonical go, better tested, has new features, many bugfixes, and more (though there is always more to come on the roadmap).

Some notable features:
- [x] Parse multi-frame DICOM imagery (both encapsulated and native pixel data)
- [x] Channel-based streaming of `Frame`s to a client _as they are parsed_ out of the dicom
- [x] Cleaner Go Element and Dataset representations (in the absence of Go generics)
- [x] Better support for icon image sets in addition to primary image sets
- [x] Write and encode Datasets back to DICOM files
- [x] Enhanced testing and benchmarking support
- [x] Modern, canonical Go.

## Usage
To use this in your golang project, import `github.com/suyashkumar/dicom`. This repository supports Go modules, and regularly tags releases using semantic versioning. Typical usage is straightforward:
```go 

dataset, _ := dicom.ParseFile("testdata/1.dcm", nil) // See also: dicom.Parse which has a generic io.Reader API.

// Dataset will nicely print the DICOM dataset data out of the box.
fmt.Println(dataset)

// Dataset is also JSON serializable out of the box.
j, _ := json.Marshal(dataset)
fmt.Println(j)
```
More details about the package (and additional examples and APIs) can be found in the [godoc](https://godoc.org/github.com/suyashkumar/dicom).

## Structural Validation

Before writing a dataset you can produce a standalone, read-only structural check report with `Dataset.Validate` (or the package-level `dicom.ValidateDataset`). It compares each element against the data dictionary and the DICOM PS3.5 representation rules:

- reports the concrete value representation (VR), in-memory value shape, and value multiplicity (VM) violations, covering fixed (`1`), interval (`1-3`, `1-n`), and multiple (`2-2n`) VM forms;
- checks PS3.5 representation ranges for numeric VRs (US/SS/UL/SL), string lengths for character VRs (measured in characters after stripping insignificant trailing padding), and word-width alignment for binary VRs — binary values are a single octet stream and never have their VM derived from the byte length;
- recurses into Sequence items and reports the full tag/item path, e.g. `(0008,1199)/item[1]/(0040,a043)/item[2]/(0008,0100)`;
- returns findings it cannot prove as **undetermined** rather than as successes: unknown/non-standard tags, unknown private fields, opaque or context-dependent VRs (e.g. `UN`, or multi-VR tags such as OB/OW without a concrete VR), and unrecognized VM strings;
- caps the number of aggregated findings (default `dicom.DefaultMaxIssues = 100`, configurable via `dicom.WithMaxIssues`; `<= 0` means unlimited) and sets `Report.Truncated` when findings were dropped.

```go
report := dataset.Validate() // or dicom.ValidateDataset(&dataset, dicom.WithMaxIssues(200))
if !report.Valid() {
    // report.Errors() are definite structural violations;
    // report.Undetermined() could not be judged from available evidence.
    for _, issue := range report.Issues {
        fmt.Println(issue)
    }
}
```

Validation never modifies the dataset (no values are repaired or re-encoded). It verifies file structure only: it does not assert any Information Object Definition (IOD), Type 1/Type 2 presence, or medical/diagnostic compliance.

## CLI Tool
A CLI tool that uses this package to parse imagery and metadata out of DICOMs is provided in the `cmd/dicomutil` package. This tool can take in a DICOM, and dump out all the elements to STDOUT, in addition to writing out any imagery to the current working directory either as PNGs or JPEG (note, it does not perform any automatic color rescaling by default).

### Installation
You can download the prebuilt binaries from the [releases tab](https://github.com/suyashkumar/dicom/releases), or use the following to download the binary at the command line using my [getbin tool](https://github.com/suyashkumar/getbin):

```sh
wget -qO- "https://getbin.io/suyashkumar/dicom" | tar xvz
```
(This attempts to infer your OS and 301 redirects `wget` to the latest github release asset for your system. Downloads come from GitHub releases).

### Usage
```
dicomutil -path myfile.dcm
```
Note: for some DICOMs (with native pixel data) no automatic intensity scaling is applied yet (this is coming). You can apply this in your image viewer if needed (in Preview on mac, go to Tools->Adjust Color). 


### Build manually
To build manually, ensure you have `make` and `go` installed. Clone (or `go get`) this repo into your `$GOPATH` and then simply run:
```sh
make
```
Which will build the dicomutil binary and include it in a `build/` folder in your current working directory. 

You can also built it using Go directly:

```sh
go build -o dicomutil ./cmd/dicomutil
```

## History
Here's a little more history on this repository for those who are interested! 

### v0
The v0 [suyashkumar/dicom](https://github.com/suyashkumar/dicom) started off as a hard fork of [go-dicom](https://github.com/gillesdemey/go-dicom) which was not being maintained actively anymore (with the [original author being supportive of my fork](https://www.reddit.com/r/golang/comments/bnu47l/high_performance_dicom_medical_image_parser_in/en9hp6h?utm_source=share&utm_medium=web2x&context=3)--thank you!). I worked on adding several new capabilities, bug fixes, and general maintainability refactors (like multiframe support, streaming parsing, updated APIs, low-level parsing bug fixes, and more).

That represents the __v0__ history of the repository. 

### v1

For __v1__ I rewrote and redesigned the core library essentially from scratch, and added several new features and bug fixes that only live in __v1__. The architecture and APIs are completely different, as is some of the underlying parser logic (to be more efficient and correct). Most of the core rewrite work happened at the [`s/1.0-rewrite`](https://github.com/suyashkumar/dicom/tree/s/1.0-rewrite) branch. 


## Acknowledgements

* <img src="https://user-images.githubusercontent.com/6299853/90325771-b23f2e80-df34-11ea-9d18-5c33b69c2746.png" width="110px" align="left"/> [Segmed](https://www.segmed.ai/) for their help with validation and other contributions to the library. 
* Original [go-dicom](https://github.com/gillesdemey/go-dicom)
* Grailbio [go-dicom](https://github.com/grailbio/go-dicom) -- commits from their fork were applied to ours
* GradientHealth for supporting work I did on this while there [gradienthealth/dicom](https://github.com/gradienthealth/dicom)
* Innolitics [DICOM browser](https://dicom.innolitics.com/ciods)
* [DICOM Specification](https://dicom.nema.org/medical/dicom/current/output/pdf/part05.pdf)
* <div>Icons made by <a href="https://www.freepik.com/?__hstc=57440181.48e262e7f01bcb2b41259e2e5a8103b3.1557697512782.1557697512782.1557697512782.1&__hssc=57440181.4.1557697512783&__hsfp=2768524783" title="Freepik">Freepik</a> from <a href="https://www.flaticon.com/" title="Flaticon">www.flaticon.com</a> is licensed by <a href="https://creativecommons.org/licenses/by/3.0/" title="Creative Commons BY 3.0" target="_blank">CC 3.0 BY</a></div>
