// Minimal UEFI definitions for tools/efiboot (AArch64, UEFI 2.x). Only what
// the loader uses; layouts from the UEFI specification.

typedef unsigned char u8;
typedef unsigned short u16;
typedef unsigned int u32;
typedef unsigned long long u64;
typedef u64 uintn;
typedef u64 efi_status;
typedef void *efi_handle;
typedef u16 char16;

#define EFI_SUCCESS 0
#define EFI_BUFFER_TOO_SMALL (0x8000000000000000ULL | 5)
#define EFI_ERROR(s) ((long long)(s) < 0)

typedef struct {
	u32 d1;
	u16 d2, d3;
	u8 d4[8];
} efi_guid;

typedef struct {
	u64 signature;
	u32 revision, header_size, crc32, reserved;
} efi_table_header;

typedef struct efi_text_out {
	efi_status (*reset)(struct efi_text_out *, u8);
	efi_status (*output_string)(struct efi_text_out *, const char16 *);
	void *test_string, *query_mode, *set_mode, *set_attribute;
	efi_status (*clear_screen)(struct efi_text_out *);
	void *set_cursor_position, *enable_cursor;
	void *mode;
} efi_text_out;

typedef struct {
	u32 type, pad;
	u64 physical_start, virtual_start, number_of_pages, attribute;
} efi_memory_descriptor;

enum { AllocateAnyPages, AllocateMaxAddress, AllocateAddress };
enum { EfiLoaderData = 2, EfiConventionalMemory = 7 };

typedef struct {
	efi_table_header hdr;
	void *raise_tpl, *restore_tpl;
	efi_status (*allocate_pages)(int type, int memtype, uintn pages, u64 *addr);
	efi_status (*free_pages)(u64 addr, uintn pages);
	efi_status (*get_memory_map)(uintn *size, efi_memory_descriptor *map, uintn *key, uintn *desc_size, u32 *desc_version);
	efi_status (*allocate_pool)(int memtype, uintn size, void **buf);
	efi_status (*free_pool)(void *buf);
	void *create_event, *set_timer, *wait_for_event, *signal_event, *close_event, *check_event;
	void *install_protocol_interface, *reinstall_protocol_interface, *uninstall_protocol_interface;
	efi_status (*handle_protocol)(efi_handle, const efi_guid *, void **);
	void *reserved;
	void *register_protocol_notify, *locate_handle, *locate_device_path, *install_configuration_table;
	void *load_image, *start_image, *exit, *unload_image;
	efi_status (*exit_boot_services)(efi_handle, uintn key);
	void *get_next_monotonic_count;
	efi_status (*stall)(uintn us);
	efi_status (*set_watchdog_timer)(uintn timeout, u64 code, uintn size, char16 *data);
	void *connect_controller, *disconnect_controller;
	void *open_protocol, *close_protocol, *open_protocol_information;
	void *protocols_per_handle, *locate_handle_buffer;
	efi_status (*locate_protocol)(const efi_guid *, void *registration, void **iface);
} efi_boot_services;

typedef struct {
	efi_guid guid;
	void *table;
} efi_configuration_table;

typedef struct {
	efi_table_header hdr;
	char16 *firmware_vendor;
	u32 firmware_revision, pad;
	efi_handle console_in_handle;
	void *con_in;
	efi_handle console_out_handle;
	efi_text_out *con_out;
	efi_handle standard_error_handle;
	efi_text_out *std_err;
	void *runtime_services;
	efi_boot_services *boot_services;
	uintn number_of_table_entries;
	efi_configuration_table *configuration_table;
} efi_system_table;

typedef struct {
	u32 revision, pad;
	efi_handle parent_handle;
	efi_system_table *system_table;
	efi_handle device_handle;
	void *file_path, *reserved;
	u32 load_options_size, pad2;
	void *load_options;
	void *image_base;
	u64 image_size;
} efi_loaded_image;

typedef struct efi_file {
	u64 revision;
	efi_status (*open)(struct efi_file *, struct efi_file **, const char16 *name, u64 mode, u64 attr);
	efi_status (*close)(struct efi_file *);
	void *delete_;
	efi_status (*read)(struct efi_file *, uintn *size, void *buf);
	void *write;
	void *get_position, *set_position;
	efi_status (*get_info)(struct efi_file *, const efi_guid *, uintn *size, void *buf);
} efi_file;

typedef struct efi_simple_fs {
	u64 revision;
	efi_status (*open_volume)(struct efi_simple_fs *, efi_file **root);
} efi_simple_fs;

typedef struct {
	u64 size, file_size, physical_size;
	u8 times[48];
	u64 attribute;
	char16 name[];
} efi_file_info;

typedef struct {
	u32 version, horizontal_resolution, vertical_resolution;
	u32 pixel_format; // 0 RGBX, 1 BGRX, 2 bitmask, 3 blt only
	u32 red_mask, green_mask, blue_mask, reserved_mask;
	u32 pixels_per_scan_line;
} efi_gop_mode_info;

typedef struct {
	u32 max_mode, mode;
	efi_gop_mode_info *info;
	uintn size_of_info;
	u64 frame_buffer_base;
	uintn frame_buffer_size;
} efi_gop_mode;

typedef struct {
	void *query_mode, *set_mode, *blt;
	efi_gop_mode *mode;
} efi_gop;

typedef struct efi_rng {
	void *get_info;
	efi_status (*get_rng)(struct efi_rng *, const efi_guid *alg, uintn len, u8 *buf);
} efi_rng;

#define GUID(a, b, c, d0, d1, d2, d3, d4, d5, d6, d7) {a, b, c, {d0, d1, d2, d3, d4, d5, d6, d7}}

static const efi_guid ACPI20_GUID = GUID(0x8868e871, 0xe4f1, 0x11d3, 0xbc, 0x22, 0x00, 0x80, 0xc7, 0x3c, 0x88, 0x81);
static const efi_guid FDT_GUID = GUID(0xb1b621d5, 0xf19c, 0x44a5, 0x99, 0x48, 0x76, 0x14, 0x5b, 0x0d, 0xd0, 0x41);
static const efi_guid LOADED_IMAGE_GUID = GUID(0x5b1b31a1, 0x9562, 0x11d2, 0x8e, 0x3f, 0x00, 0xa0, 0xc9, 0x69, 0x72, 0x3b);
static const efi_guid SIMPLE_FS_GUID = GUID(0x964e5b22, 0x6459, 0x11d2, 0x8e, 0x39, 0x00, 0xa0, 0xc9, 0x69, 0x72, 0x3b);
static const efi_guid FILE_INFO_GUID = GUID(0x09576e92, 0x6d3f, 0x11d2, 0x8e, 0x39, 0x00, 0xa0, 0xc9, 0x69, 0x72, 0x3b);
static const efi_guid GOP_GUID = GUID(0x9042a9de, 0x23dc, 0x4a38, 0x96, 0xfb, 0x7a, 0xde, 0xd0, 0x80, 0x51, 0x6a);
static const efi_guid RNG_GUID = GUID(0x3152bca5, 0xeade, 0x433d, 0x86, 0x2e, 0xc0, 0x1c, 0xdc, 0x29, 0x1f, 0x44);
