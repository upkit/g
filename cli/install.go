package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	ct "github.com/daviddengcn/go-colortext"
	"github.com/dixonwille/wlog/v3"
	"github.com/dixonwille/wmenu/v5"
	"github.com/mholt/archiver/v3"
	"github.com/urfave/cli/v2"
	"github.com/voidint/g/collector"
	"github.com/voidint/g/version"
)

func install(ctx *cli.Context) (err error) {
	vname := ctx.Args().First()
	if vname == "" {
		return cli.ShowSubcommandHelp(ctx)
	}

	// 查找版本
	c, err := collector.NewCollector(strings.Split(os.Getenv(mirrorEnv), mirrorSep)...)
	if err != nil {
		return cli.Exit(errstring(err), 1)
	}
	items, err := c.AllVersions()
	if err != nil {
		return cli.Exit(errstring(err), 1)
	}

	v, err := version.NewFinder(items,
		version.WithFinderPackageKind(version.ArchiveKind),
		version.WithFinderGoos(runtime.GOOS),
		version.WithFinderGoarch(runtime.GOARCH),
	).Find(vname)
	if err != nil {
		return cli.Exit(errstring(err), 1)
	}

	vname = v.Name()
	targetV := filepath.Join(versionsDir, vname)

	// 检查版本是否已经安装
	if finfo, err := os.Stat(targetV); err == nil && finfo.IsDir() {
		return cli.Exit(fmt.Sprintf("[g] %q version has been installed.", vname), 1)
	}

	// 查找版本下当前平台的安装包
	pkgs, err := v.FindPackages(version.ArchiveKind, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return cli.Exit(errstring(err), 1)
	}
	var pkg version.Package
	if len(pkgs) > 1 {
		menu := wmenu.NewMenu("Please select the package you want to install.")
		menu.AddColor(
			wlog.Color{Code: ct.Green},
			wlog.Color{Code: ct.Yellow},
			wlog.Color{Code: ct.Magenta},
			wlog.Color{Code: ct.Yellow},
		)
		menu.Action(func(opts []wmenu.Opt) error {
			pkg = opts[0].Value.(version.Package)
			return nil
		})
		for i := range pkgs {
			if i == 0 {
				menu.Option(pkgs[i].FileName, pkgs[i], true, nil)
			} else {
				menu.Option(" "+pkgs[i].FileName, pkgs[i], false, nil)
			}
		}
		if err = menu.Run(); err != nil {
			return cli.Exit(errstring(err), 1)
		}
	} else {
		pkg = pkgs[0]
	}

	var checksumNotFound, skipChecksum bool
	if pkg.Checksum == "" && pkg.ChecksumURL == "" {
		checksumNotFound = true
		menu := wmenu.NewMenu("Checksum file not found, do you want to continue?")
		menu.IsYesNo(wmenu.DefN)
		menu.Action(func(opts []wmenu.Opt) error {
			skipChecksum = opts[0].Value.(string) == "yes"
			return nil
		})
		if err = menu.Run(); err != nil {
			return cli.Exit(errstring(err), 1)
		}
	}
	if checksumNotFound && !skipChecksum {
		return
	}

	var ext string
	if runtime.GOOS == "windows" {
		ext = "zip"
	} else {
		ext = "tar.gz"
	}
	filename := filepath.Join(downloadsDir, fmt.Sprintf("go%s.%s-%s.%s", vname, runtime.GOOS, runtime.GOARCH, ext))

	if _, err = os.Stat(filename); os.IsNotExist(err) {
		// 本地不存在安装包，从远程下载并检查校验和。
		if _, err = pkg.DownloadWithProgress(filename); err != nil {
			return cli.Exit(errstring(err), 1)
		}

		if !skipChecksum {
			fmt.Println("Computing checksum with", pkg.Algorithm)
			if err = pkg.VerifyChecksum(filename); err != nil {
				return cli.Exit(errstring(err), 1)
			}
			fmt.Println("Checksums matched")
		}

	} else {
		if !skipChecksum {
			// 本地存在安装包，检查校验和。
			fmt.Println("Computing checksum with", pkg.Algorithm)
			if err = pkg.VerifyChecksum(filename); err != nil {
				_ = os.Remove(filename)
				return cli.Exit(errstring(err), 1)
			}
			fmt.Println("Checksums matched")
		}
	}

	// 删除可能存在的历史垃圾文件
	_ = os.RemoveAll(filepath.Join(versionsDir, "go"))

	// 解压安装包
	if err = archiver.Unarchive(filename, versionsDir); err != nil {
		return cli.Exit(errstring(err), 1)
	}
	// 目录重命名
	if err = os.Rename(filepath.Join(versionsDir, "go"), targetV); err != nil {
		return cli.Exit(errstring(err), 1)
	}

	if ctx.Bool("nouse") {
		return nil
	}

	// 重新建立软链接
	_ = os.Remove(goroot)

	if err = mkSymlink(targetV, goroot); err != nil {
		return cli.Exit(errstring(err), 1)
	}

	// 如果开启了拷贝模式，重新拷贝一份新版本
	if gcopy() {
		_ = os.RemoveAll(copyroot)

		if err = copyDir(targetV, copyroot); err != nil {
			return cli.Exit(errstring(err), 1)
		}
	}

	fmt.Printf("Now using go%s\n", v.Name())
	return nil
}

func mkSymlink(oldname, newname string) (err error) {
	if runtime.GOOS == "windows" {
		// Windows 10下无特权用户无法创建符号链接，优先调用mklink /j创建'目录联接'
		if err = exec.Command("cmd", "/c", "mklink", "/j", newname, oldname).Run(); err == nil {
			return nil
		}
	}
	return os.Symlink(oldname, newname)
}

// copyDir 拷贝一个目录及其子目录和文件到另一个目录
func copyDir(src string, dest string) error {
	// 获取源目录的属性
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}

	// 创建目标目录
	err = os.MkdirAll(dest, srcInfo.Mode())
	if err != nil {
		return err
	}

	// 读取源目录下的所有文件和子目录
	dir, err := os.Open(src)
	if err != nil {
		return err
	}
	defer dir.Close()

	files, err := dir.Readdir(-1)
	if err != nil {
		return err
	}

	for _, file := range files {
		srcPath := filepath.Join(src, file.Name())
		destPath := filepath.Join(dest, file.Name())

		if file.IsDir() {
			// 如果是子目录，递归拷贝
			err = copyDir(srcPath, destPath)
			if err != nil {
				return err
			}
		} else {
			// 如果是文件，直接拷贝
			err = copyFile(srcPath, destPath)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// copyFile 拷贝一个文件到另一个文件
func copyFile(src string, dest string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	destFile, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer destFile.Close()

	_, err = io.Copy(destFile, srcFile) // 拷贝数据
	if err != nil {
		return err
	}

	srcInfo, err := os.Stat(src) // 获取源文件的属性
	if err != nil {
		return err
	}
	err = os.Chmod(dest, srcInfo.Mode()) // 设置目标文件的属性和源文件一致
	if err != nil {
		return err
	}

	return nil
}
