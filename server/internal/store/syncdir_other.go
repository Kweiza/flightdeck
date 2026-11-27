//go:build !unix

package store

// syncDir 는 비유닉스에서 할 일이 없다. Windows 는 디렉토리를 열어 fsync 하는 길이 없고(FlushFileBuffers 가 디렉토리
// 핸들을 거절한다), NTFS 는 이름 붙이기 같은 메타데이터를 자기 저널로 굳힌다. 오류를 내면 그 플랫폼에서 역이관이 늘
// 실패한다 - 못 재는 것이 아니라 잴 것이 없는 자리라 nil 이다.
func syncDir(string) error { return nil }
