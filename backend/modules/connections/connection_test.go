package connections

import (
	"errors"
	"testing"
)

func validTestInput() Input {
	return Input{
		Name:         "Test Database",
		Host:         "localhost",
		Port:         5432,
		DatabaseName: "test_db",
		Username:     "test_user",
		Password:     "StrongPassword123!",
		SSLMode:      "require",
	}
}

func TestValidateValidInput(t *testing.T) {
	input := validTestInput()

	if err := validate(input, true); err != nil {
		t.Fatalf("validate() error = %v, want nil", err)
	}
}

func TestValidateInvalidInput(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Input)
	}{
		{
			name: "empty name",
			mutate: func(i *Input) {
				i.Name = ""
			},
		},
		{
			name: "name with surrounding spaces",
			mutate: func(i *Input) {
				i.Name = " Test Database "
			},
		},
		{
			name: "empty host",
			mutate: func(i *Input) {
				i.Host = ""
			},
		},
		{
			name: "port too small",
			mutate: func(i *Input) {
				i.Port = 0
			},
		},
		{
			name: "port too large",
			mutate: func(i *Input) {
				i.Port = 65536
			},
		},
		{
			name: "unsupported ssl mode",
			mutate: func(i *Input) {
				i.SSLMode = "invalid"
			},
		},
		{
			name: "password required for create",
			mutate: func(i *Input) {
				i.Password = ""
			},
		},
		{
			name: "password too long",
			mutate: func(i *Input) {
				i.Password = string(make([]byte, 1025))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := validTestInput()
			tt.mutate(&input)

			if err := validate(input, true); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("validate() error = %v, want ErrInvalidInput", err)
			}
		})
	}
}

func TestValidateUpdateCanOmitPassword(t *testing.T) {
	input := validTestInput()
	input.Password = ""

	if err := validate(input, false); err != nil {
		t.Fatalf("update should allow an omitted password, got %v", err)
	}
}

func TestEncryptDecryptPassword(t *testing.T) {
	repo := NewRepository(nil, "test-secret")
	password := "StrongPassword123!"

	encrypted, err := repo.encrypt(password)
	if err != nil {
		t.Fatalf("encrypt() error = %v", err)
	}
	if len(encrypted) == 0 {
		t.Fatal("encrypted password must not be empty")
	}
	if string(encrypted) == password {
		t.Fatal("encrypted password must not equal plaintext")
	}

	decrypted, err := repo.decrypt(encrypted)
	if err != nil {
		t.Fatalf("decrypt() error = %v", err)
	}
	if decrypted != password {
		t.Fatalf("decrypted password = %q, want original password", decrypted)
	}
}

func TestDecryptWithWrongKeyFails(t *testing.T) {
	repo1 := NewRepository(nil, "secret-one")
	repo2 := NewRepository(nil, "secret-two")

	encrypted, err := repo1.encrypt("password")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := repo2.decrypt(encrypted); err == nil {
		t.Fatal("decrypt with wrong key should fail")
	}
}

func TestDecryptInvalidCiphertextFails(t *testing.T) {
	repo := NewRepository(nil, "test-secret")

	if _, err := repo.decrypt([]byte("invalid")); err == nil {
		t.Fatal("decrypt should reject invalid ciphertext")
	}
}

func pw(s string) *string { return &s }

func roleInput() Input {
	in := validTestInput()
	in.Username = "wbd_target_a_viewer"
	in.Executor = &RoleCredential{Username: "wbd_target_a_executor", Password: pw("x")}
	in.Preparer = &RoleCredential{Username: "wbd_target_a_preparer", Password: pw("y")}
	return in
}

func TestValidateRoles(t *testing.T) {
	if err := validate(roleInput(), true); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*Input){
		"executor without preparer": func(i *Input) { i.Preparer = nil },
		"preparer without executor": func(i *Input) { i.Executor = nil },
		"create omitted password":   func(i *Input) { i.Executor.Password = nil },
		"empty password":            func(i *Input) { i.Preparer.Password = pw("") },
		"bad suffix":                func(i *Input) { i.Executor.Username = "wbd_target_a_exec" },
		"mismatched pair":           func(i *Input) { i.Preparer.Username = "wbd_target_b_preparer" },
		"blank username":            func(i *Input) { i.Executor.Username = " " },
		"viewer equals executor":    func(i *Input) { i.Username = i.Executor.Username },
	}
	for name, mutate := range cases {
		in := roleInput()
		mutate(&in)
		if err := validate(in, true); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	in := roleInput()
	in.Executor.Password = nil
	if err := validate(in, false); err != nil {
		t.Errorf("update may omit role password: %v", err)
	}
	in.Preparer.Password = pw("")
	if err := validate(in, false); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("update must reject empty role password: %v", err)
	}
	if err := validate(validTestInput(), true); err != nil {
		t.Errorf("viewer-only must stay valid: %v", err)
	}
}
