//go:build unix

package store

import "os"

// syncDir 는 디렉토리 dir 의 항목(새로 붙인 이름)을 디스크에 굳힌다(fsync).
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); cerr != nil && err == nil {
		err = cerr
	}
	return err
}
