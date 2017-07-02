package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func main() {
	if err := start(); err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(2)
	}
}

var debugEnabled = false

func start() error {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" runtime.GOOS != "darwin" {
		return fmt.Errorf("Unsupported OS '%v'", runtime.GOOS)
	}
	if runtime.GOARCH != "amd64" {
		return fmt.Errorf("Unsupported OS '%v' or arch '%v'", runtime.GOOS, runtime.GOARCH)
	}
	if len(os.Args) < 2 {
		return fmt.Errorf("No command provided")
	}

	switch os.Args[1] {
	case "rerun":
		err := clean()
		if err == nil {
			err = run()
		}
		return err
	case "run":
		return run()
	case "clean":
		return clean()
	case "rebuild":
		err := clean()
		if err == nil {
			err = build()
		}
		return err
	case "build":
		return build()
	case "package":
		return pkg()
	case "build-cef":
		return buildCef()
	default:
		return fmt.Errorf("Unrecognized command '%v'", os.Args[1])
	}
}

func run() error {
	if err := build(); err != nil {
		return err
	}
	target, err := target()
	if err != nil {
		return err
	}
	return execCmd(filepath.Join(target, exeExt("qt_cef_poc")))
}

func clean() error {
	err := os.RemoveAll("debug")
	if err == nil {
		err = os.RemoveAll("release")
	}
	return err
}

func build() error {
	target, err := target()
	if err != nil {
		return err
	}
	// Get qmake path
	qmakePath, err := exec.LookPath(exeExt("qmake"))
	if err != nil {
		return err
	}

	// Make the dir for the target
	if err := os.MkdirAll(target, 0755); err != nil {
		return err
	}

	// Run qmake TODO: put behind flag
	qmakeArgs := []string{}
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		if target == "debug" {
			qmakeArgs = []string{"CONFIG+=debug"}
		} else {
			qmakeArgs = []string{"CONFIG+=release", "CONFIG-=debug"}
		}
	}
	qmakeArgs = append(qmakeArgs, "qt_cef_poc.pro")
	if err := execCmd(qmakePath, qmakeArgs...); err != nil {
		return fmt.Errorf("QMake failed: %v", err)
	}

	// Run nmake if windows, make if linux
	makeExe := "make"
	if runtime.GOOS == "windows" {
		makeExe = "nmake.exe"
	}
	if err := execCmd(makeExe, target); err != nil {
		return fmt.Errorf("NMake failed: %v", err)
	}

	// Chmod on linux
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		if err = os.Chmod(filepath.Join(target, "qt_cef_poc"), 0755); err != nil {
			return err
		}
	}

	// Copy over resources
	if err := copyResources(qmakePath, target); err != nil {
		return err
	}

	return nil
}

func pkg() error {
	target, err := target()
	if err != nil {
		return err
	}
	// Just move over the files that matter to a new deploy dir and zip em up
	deployDir := filepath.Join(target, "package", "qt_cef_poc")
	if err = os.MkdirAll(deployDir, 0755); err != nil {
		return err
	}

	// Get all base-dir items to copy, excluding only some
	filesToCopy := []string{}
	dirFiles, err := ioutil.ReadDir(target)
	if err != nil {
		return err
	}
	for _, file := range dirFiles {
		if !file.IsDir() {
			switch filepath.Ext(file.Name()) {
			case ".cpp", ".h", ".obj", ".res", ".manifest", ".log":
				// No-op
			default:
				filesToCopy = append(filesToCopy, file.Name())
			}
		}
	}
	if err = copyEachToDirIfNotPresent(target, deployDir, filesToCopy...); err != nil {
		return err
	}

	// And the locales dir
	if err = os.MkdirAll(filepath.Join(deployDir, "locales"), 0755); err != nil {
		return err
	}
	err = copyEachToDirIfNotPresent(filepath.Join(target, "locales"), filepath.Join(deployDir, "locales"), "en-US.pak")
	if err != nil {
		return err
	}

	// Now create a zip or tar file with all the goods
	if runtime.GOOS == "windows" {
		err = createSingleDirZipFile(deployDir, filepath.Join(target, "package", "qt_cef_poc.zip"))
	} else {
		err = createSingleDirTarGzFile(deployDir, filepath.Join(target, "package", "qt_cef_poc.tar.gz"))
	}
	if err != nil {
		return err
	}

	return os.RemoveAll(deployDir)
}

func buildCef() error {
	if runtime.GOOS == "windows" {
		return buildCefWindows()
	}
	return buildCefLinux()
}

func buildCefLinux() error {
	cefDir := os.Getenv("CEF_DIR")
	if cefDir == "" {
		return fmt.Errorf("Unable to find CEF_DIR env var")
	}
	// We have to run separate make runs for different target types
	makeLib := func(target string) error {
		if err := execCmdInDir(cefDir, "cmake", "-DCMAKE_BUILD_TYPE="+target, "."); err != nil {
			return fmt.Errorf("CMake failed: %v", err)
		}
		wrapperDir := filepath.Join(cefDir, "libcef_dll_wrapper")
		if err := execCmdInDir(wrapperDir, "make"); err != nil {
			return fmt.Errorf("Make failed: %v", err)
		}
		if err := os.Rename(filepath.Join(wrapperDir, "libcef_dll_wrapper.a"),
			filepath.Join(wrapperDir, "libcef_dll_wrapper_"+target+".a")); err != nil {
			return fmt.Errorf("Unable to rename .a file: %v", err)
		}
		return nil
	}

	if err := makeLib("Debug"); err != nil {
		return err
	}
	return makeLib("Release")
}

func buildCefWindows() error {
	cefDir := os.Getenv("CEF_DIR")
	if cefDir == "" {
		return fmt.Errorf("Unable to find CEF_DIR env var")
	}
	// Build the make files
	if err := execCmdInDir(cefDir, "cmake", "-G", "Visual Studio 14 Win64", "."); err != nil {
		return fmt.Errorf("CMake failed: %v", err)
	}

	// Replace a couple of strings in VC proj file on windows
	dllWrapperDir := filepath.Join(cefDir, "libcef_dll_wrapper")
	vcProjFile := filepath.Join(dllWrapperDir, "libcef_dll_wrapper.vcxproj")
	projXml, err := ioutil.ReadFile(vcProjFile)
	if err != nil {
		return fmt.Errorf("Unable to read VC proj file: %v", err)
	}
	// First one is debug, second is release
	projXml = bytes.Replace(projXml, []byte("<RuntimeLibrary>MultiThreaded</RuntimeLibrary>"),
		[]byte("<RuntimeLibrary>MultiThreadedDebugDLL</RuntimeLibrary>"), 1)
	projXml = bytes.Replace(projXml, []byte("<RuntimeLibrary>MultiThreaded</RuntimeLibrary>"),
		[]byte("<RuntimeLibrary>MultiThreadedDLL</RuntimeLibrary>"), 1)
	if err = ioutil.WriteFile(vcProjFile, projXml, os.ModePerm); err != nil {
		return fmt.Errorf("Unable to write VC proj file: %v", err)
	}

	// Build debug and then build release
	if err = execCmdInDir(dllWrapperDir, "msbuild", "libcef_dll_wrapper.vcxproj", "/p:Configuration=Debug"); err != nil {
		return fmt.Errorf("Unable to build debug wrapper: %v", err)
	}
	if err = execCmdInDir(dllWrapperDir, "msbuild", "libcef_dll_wrapper.vcxproj", "/p:Configuration=Release"); err != nil {
		return fmt.Errorf("Unable to build release wrapper: %v", err)
	}
	return nil
}

func target() (string, error) {
	target := "debug"
	if len(os.Args) >= 3 {
		if os.Args[2] != "release" && os.Args[2] != "debug" {
			return "", fmt.Errorf("Unknown target '%v'", os.Args[2])
		}
		target = os.Args[2]
	}
	return target, nil
}

func exeExt(baseName string) string {
	if runtime.GOOS == "windows" {
		return baseName + ".exe"
	}
	return baseName
}

func execCmd(name string, args ...string) error {
	return execCmdInDir("", name, args...)
}

func execCmdInDir(dir string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func copyResources(qmakePath string, target string) error {
	if runtime.GOOS == "windows" {
		return copyResourcesWindows(qmakePath, target)
	}
	return copyResourcesLinux(qmakePath, target)
}

func copyResourcesLinux(qmakePath string, target string) error {
	cefDir := os.Getenv("CEF_DIR")
	if cefDir == "" {
		return fmt.Errorf("Unable to find CEF_DIR env var")
	}
	// Everything read only except by owner
	// Copy over some Qt DLLs
	qmakeLibPath, _ := filepath.Rel(filepath.Dir(qmakePath), "../lib")
	err := copyAndChmodEachToDirIfNotPresent(0644, qmakeLibPath, target,
		"libQt5Core.so",
		"libQt5Gui.so",
		"libQt5Widgets.so",
	)
	if err != nil {
		return err
	}

	// Copy over CEF libs
	err = copyAndChmodEachToDirIfNotPresent(0644, filepath.Join(cefDir, strings.Title(target)), target,
		"libcef.so",
		"natives_blob.bin",
		"snapshot_blob.bin",
	)
	if err != nil {
		return err
	}

	// Copy over CEF resources
	cefResDir := filepath.Join(cefDir, "Resources")
	err = copyAndChmodEachToDirIfNotPresent(0644, cefResDir, target,
		"icudtl.dat",
		"cef.pak",
		"cef_100_percent.pak",
		"cef_200_percent.pak",
		"cef_extensions.pak",
		"devtools_resources.pak",
	)
	if err != nil {
		return err
	}

	// And CEF locales
	targetLocaleDir := filepath.Join(target, "locales")
	if err = os.MkdirAll(targetLocaleDir, 0644); err != nil {
		return err
	}
	err = copyAndChmodEachToDirIfNotPresent(0644, filepath.Join(cefResDir, "locales"), targetLocaleDir, "en-US.pak")
	return err
}

func copyResourcesWindows(qmakePath string, target string) error {
	cefDir := os.Getenv("CEF_DIR")
	if cefDir == "" {
		return fmt.Errorf("Unable to find CEF_DIR env var")
	}
	// Copy over some Qt DLLs
	qtDlls := []string{
		"Qt5Core.dll",
		"Qt5Gui.dll",
		"Qt5Widgets.dll",
	}
	// Debug libs are d.dll
	if target == "debug" {
		for i := range qtDlls {
			qtDlls[i] = strings.Replace(qtDlls[i], ".dll", "d.dll", -1)
		}
	}
	err := copyEachToDirIfNotPresent(filepath.Dir(qmakePath), target, qtDlls...)
	if err != nil {
		return err
	}

	// Need special ucrtbased.dll for debug builds
	if target == "debug" {
		err = copyEachToDirIfNotPresent("C:\\Program Files (x86)\\Windows Kits\\10\\bin\\x64\\ucrt",
			target, "ucrtbased.dll")
		if err != nil {
			return err
		}
	}

	// Copy over CEF libs
	err = copyEachToDirIfNotPresent(filepath.Join(cefDir, strings.Title(target)), target,
		"libcef.dll",
		"chrome_elf.dll",
		"natives_blob.bin",
		"snapshot_blob.bin",
		"d3dcompiler_43.dll",
		"d3dcompiler_47.dll",
		"libEGL.dll",
		"libGLESv2.dll",
	)
	if err != nil {
		return err
	}

	// Copy over CEF resources
	cefResDir := filepath.Join(cefDir, "Resources")
	err = copyEachToDirIfNotPresent(cefResDir, target,
		"icudtl.dat",
		"cef.pak",
		"cef_100_percent.pak",
		"cef_200_percent.pak",
		"cef_extensions.pak",
		"devtools_resources.pak",
	)
	if err != nil {
		return err
	}

	// And CEF locales
	targetLocaleDir := filepath.Join(target, "locales")
	if err = os.MkdirAll(targetLocaleDir, 0755); err != nil {
		return err
	}
	err = copyEachToDirIfNotPresent(filepath.Join(cefResDir, "locales"), targetLocaleDir, "en-US.pak")
	return err
}

func chmodEachInDir(mode os.FileMode, dir string, filenames ...string) error {
	for _, filename := range filenames {
		if err := os.Chmod(filepath.Join(dir, filename), mode); err != nil {
			return err
		}
	}
	return nil
}

func copyAndChmodEachToDirIfNotPresent(mode os.FileMode, srcDir string, destDir string, srcFilenames ...string) error {
	if err := copyEachToDirIfNotPresent(srcDir, destDir, srcFilenames...); err != nil {
		return err
	}
	return chmodEachInDir(mode, destDir, srcFilenames...)
}

func copyEachToDirIfNotPresent(srcDir string, destDir string, srcFilenames ...string) error {
	for _, srcFilename := range srcFilenames {
		if err := copyToDirIfNotPresent(filepath.Join(srcDir, srcFilename), destDir); err != nil {
			return err
		}
	}
	return nil
}

func copyToDirIfNotPresent(src string, destDir string) error {
	return copyIfNotPresent(src, filepath.Join(destDir, filepath.Base(src)))
}

func copyIfNotPresent(src string, dest string) error {
	if _, err := os.Stat(dest); os.IsExist(err) {
		debugLogf("Skipping copying '%v' to '%v' because it already exists")
		return nil
	}
	debugLogf("Copying %v to %v\n", src, dest)
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	cerr := out.Close()
	if err != nil {
		return err
	}
	return cerr
}

func debugLogf(format string, v ...interface{}) {
	if debugEnabled {
		log.Printf(format, v...)
	}
}

func createSingleDirTarGzFile(dir string, tarFilename string) error {
	tarFile, err := os.Create(tarFilename)
	if err != nil {
		return err
	}
	defer tarFile.Close()

	gw := gzip.NewWriter(tarFile)
	defer gw.Close()
	w := tar.NewWriter(gw)
	defer w.Close()

	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		tarPath := filepath.ToSlash(filepath.Join(filepath.Base(dir), rel))
		srcPath := filepath.Join(dir, rel)

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = tarPath
		if err := w.WriteHeader(header); err != nil {
			return err
		}
		src, err := os.Open(srcPath)
		if err != nil {
			return err
		}
		defer src.Close()
		_, err = io.Copy(w, src)
		return err
	})
}

func createSingleDirZipFile(dir string, zipFilename string) error {
	zipFile, err := os.Create(zipFilename)
	if err != nil {
		return err
	}
	defer zipFile.Close()

	w := zip.NewWriter(zipFile)
	defer w.Close()

	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		zipPath := filepath.ToSlash(filepath.Join(filepath.Base(dir), rel))
		srcPath := filepath.Join(dir, rel)

		dest, err := w.Create(zipPath)
		if err != nil {
			return err
		}
		src, err := os.Open(srcPath)
		if err != nil {
			return err
		}
		defer src.Close()
		_, err = io.Copy(dest, src)
		return err
	})
}
