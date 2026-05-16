import { z } from 'zod';

declare const Backend: z.ZodEnum<["local", "gdrive"]>;
type Backend = z.infer<typeof Backend>;
declare const ACLRule: z.ZodObject<{
    path: z.ZodString;
    access: z.ZodEnum<["rw", "ro", "deny"]>;
}, "strip", z.ZodTypeAny, {
    path: string;
    access: "rw" | "ro" | "deny";
}, {
    path: string;
    access: "rw" | "ro" | "deny";
}>;
type ACLRule = z.infer<typeof ACLRule>;
declare const LocalFileSystem: z.ZodObject<{
    mount: z.ZodString;
    acls: z.ZodOptional<z.ZodArray<z.ZodObject<{
        path: z.ZodString;
        access: z.ZodEnum<["rw", "ro", "deny"]>;
    }, "strip", z.ZodTypeAny, {
        path: string;
        access: "rw" | "ro" | "deny";
    }, {
        path: string;
        access: "rw" | "ro" | "deny";
    }>, "many">>;
} & {
    backend: z.ZodLiteral<"local">;
}, "strip", z.ZodTypeAny, {
    mount: string;
    backend: "local";
    acls?: {
        path: string;
        access: "rw" | "ro" | "deny";
    }[] | undefined;
}, {
    mount: string;
    backend: "local";
    acls?: {
        path: string;
        access: "rw" | "ro" | "deny";
    }[] | undefined;
}>;
type LocalFileSystem = z.infer<typeof LocalFileSystem>;
declare const GDriveFileSystem: z.ZodObject<{
    mount: z.ZodString;
    acls: z.ZodOptional<z.ZodArray<z.ZodObject<{
        path: z.ZodString;
        access: z.ZodEnum<["rw", "ro", "deny"]>;
    }, "strip", z.ZodTypeAny, {
        path: string;
        access: "rw" | "ro" | "deny";
    }, {
        path: string;
        access: "rw" | "ro" | "deny";
    }>, "many">>;
} & {
    backend: z.ZodLiteral<"gdrive">;
    gdrive_access_token: z.ZodOptional<z.ZodString>;
    gdrive_refresh_token: z.ZodOptional<z.ZodString>;
    gdrive_client_id: z.ZodOptional<z.ZodString>;
    gdrive_client_secret: z.ZodOptional<z.ZodString>;
    gdrive_service_account_json: z.ZodOptional<z.ZodString>;
    gdrive_folder_id: z.ZodOptional<z.ZodString>;
}, "strip", z.ZodTypeAny, {
    mount: string;
    backend: "gdrive";
    acls?: {
        path: string;
        access: "rw" | "ro" | "deny";
    }[] | undefined;
    gdrive_access_token?: string | undefined;
    gdrive_refresh_token?: string | undefined;
    gdrive_client_id?: string | undefined;
    gdrive_client_secret?: string | undefined;
    gdrive_service_account_json?: string | undefined;
    gdrive_folder_id?: string | undefined;
}, {
    mount: string;
    backend: "gdrive";
    acls?: {
        path: string;
        access: "rw" | "ro" | "deny";
    }[] | undefined;
    gdrive_access_token?: string | undefined;
    gdrive_refresh_token?: string | undefined;
    gdrive_client_id?: string | undefined;
    gdrive_client_secret?: string | undefined;
    gdrive_service_account_json?: string | undefined;
    gdrive_folder_id?: string | undefined;
}>;
type GDriveFileSystem = z.infer<typeof GDriveFileSystem>;
declare const FileSystem: z.ZodDiscriminatedUnion<"backend", [z.ZodObject<{
    mount: z.ZodString;
    acls: z.ZodOptional<z.ZodArray<z.ZodObject<{
        path: z.ZodString;
        access: z.ZodEnum<["rw", "ro", "deny"]>;
    }, "strip", z.ZodTypeAny, {
        path: string;
        access: "rw" | "ro" | "deny";
    }, {
        path: string;
        access: "rw" | "ro" | "deny";
    }>, "many">>;
} & {
    backend: z.ZodLiteral<"local">;
}, "strip", z.ZodTypeAny, {
    mount: string;
    backend: "local";
    acls?: {
        path: string;
        access: "rw" | "ro" | "deny";
    }[] | undefined;
}, {
    mount: string;
    backend: "local";
    acls?: {
        path: string;
        access: "rw" | "ro" | "deny";
    }[] | undefined;
}>, z.ZodObject<{
    mount: z.ZodString;
    acls: z.ZodOptional<z.ZodArray<z.ZodObject<{
        path: z.ZodString;
        access: z.ZodEnum<["rw", "ro", "deny"]>;
    }, "strip", z.ZodTypeAny, {
        path: string;
        access: "rw" | "ro" | "deny";
    }, {
        path: string;
        access: "rw" | "ro" | "deny";
    }>, "many">>;
} & {
    backend: z.ZodLiteral<"gdrive">;
    gdrive_access_token: z.ZodOptional<z.ZodString>;
    gdrive_refresh_token: z.ZodOptional<z.ZodString>;
    gdrive_client_id: z.ZodOptional<z.ZodString>;
    gdrive_client_secret: z.ZodOptional<z.ZodString>;
    gdrive_service_account_json: z.ZodOptional<z.ZodString>;
    gdrive_folder_id: z.ZodOptional<z.ZodString>;
}, "strip", z.ZodTypeAny, {
    mount: string;
    backend: "gdrive";
    acls?: {
        path: string;
        access: "rw" | "ro" | "deny";
    }[] | undefined;
    gdrive_access_token?: string | undefined;
    gdrive_refresh_token?: string | undefined;
    gdrive_client_id?: string | undefined;
    gdrive_client_secret?: string | undefined;
    gdrive_service_account_json?: string | undefined;
    gdrive_folder_id?: string | undefined;
}, {
    mount: string;
    backend: "gdrive";
    acls?: {
        path: string;
        access: "rw" | "ro" | "deny";
    }[] | undefined;
    gdrive_access_token?: string | undefined;
    gdrive_refresh_token?: string | undefined;
    gdrive_client_id?: string | undefined;
    gdrive_client_secret?: string | undefined;
    gdrive_service_account_json?: string | undefined;
    gdrive_folder_id?: string | undefined;
}>]>;
type FileSystem = z.infer<typeof FileSystem>;
declare const HttpMethod: z.ZodEnum<["GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"]>;
type HttpMethod = z.infer<typeof HttpMethod>;
declare const EgressRule: z.ZodObject<{
    host: z.ZodString;
    ports: z.ZodOptional<z.ZodArray<z.ZodNumber, "many">>;
    methods: z.ZodOptional<z.ZodArray<z.ZodEnum<["GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"]>, "many">>;
    paths: z.ZodOptional<z.ZodArray<z.ZodString, "many">>;
    headers: z.ZodOptional<z.ZodRecord<z.ZodString, z.ZodString>>;
}, "strip", z.ZodTypeAny, {
    host: string;
    ports?: number[] | undefined;
    methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
    paths?: string[] | undefined;
    headers?: Record<string, string> | undefined;
}, {
    host: string;
    ports?: number[] | undefined;
    methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
    paths?: string[] | undefined;
    headers?: Record<string, string> | undefined;
}>;
type EgressRule = z.infer<typeof EgressRule>;
declare const Egress: z.ZodObject<{
    allow: z.ZodOptional<z.ZodArray<z.ZodObject<{
        host: z.ZodString;
        ports: z.ZodOptional<z.ZodArray<z.ZodNumber, "many">>;
        methods: z.ZodOptional<z.ZodArray<z.ZodEnum<["GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"]>, "many">>;
        paths: z.ZodOptional<z.ZodArray<z.ZodString, "many">>;
        headers: z.ZodOptional<z.ZodRecord<z.ZodString, z.ZodString>>;
    }, "strip", z.ZodTypeAny, {
        host: string;
        ports?: number[] | undefined;
        methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
        paths?: string[] | undefined;
        headers?: Record<string, string> | undefined;
    }, {
        host: string;
        ports?: number[] | undefined;
        methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
        paths?: string[] | undefined;
        headers?: Record<string, string> | undefined;
    }>, "many">>;
}, "strip", z.ZodTypeAny, {
    allow?: {
        host: string;
        ports?: number[] | undefined;
        methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
        paths?: string[] | undefined;
        headers?: Record<string, string> | undefined;
    }[] | undefined;
}, {
    allow?: {
        host: string;
        ports?: number[] | undefined;
        methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
        paths?: string[] | undefined;
        headers?: Record<string, string> | undefined;
    }[] | undefined;
}>;
type Egress = z.infer<typeof Egress>;
declare const SandboxConfig: z.ZodObject<{
    image: z.ZodOptional<z.ZodString>;
    env: z.ZodOptional<z.ZodArray<z.ZodString, "many">>;
    ttl: z.ZodOptional<z.ZodNumber>;
    fs: z.ZodArray<z.ZodDiscriminatedUnion<"backend", [z.ZodObject<{
        mount: z.ZodString;
        acls: z.ZodOptional<z.ZodArray<z.ZodObject<{
            path: z.ZodString;
            access: z.ZodEnum<["rw", "ro", "deny"]>;
        }, "strip", z.ZodTypeAny, {
            path: string;
            access: "rw" | "ro" | "deny";
        }, {
            path: string;
            access: "rw" | "ro" | "deny";
        }>, "many">>;
    } & {
        backend: z.ZodLiteral<"local">;
    }, "strip", z.ZodTypeAny, {
        mount: string;
        backend: "local";
        acls?: {
            path: string;
            access: "rw" | "ro" | "deny";
        }[] | undefined;
    }, {
        mount: string;
        backend: "local";
        acls?: {
            path: string;
            access: "rw" | "ro" | "deny";
        }[] | undefined;
    }>, z.ZodObject<{
        mount: z.ZodString;
        acls: z.ZodOptional<z.ZodArray<z.ZodObject<{
            path: z.ZodString;
            access: z.ZodEnum<["rw", "ro", "deny"]>;
        }, "strip", z.ZodTypeAny, {
            path: string;
            access: "rw" | "ro" | "deny";
        }, {
            path: string;
            access: "rw" | "ro" | "deny";
        }>, "many">>;
    } & {
        backend: z.ZodLiteral<"gdrive">;
        gdrive_access_token: z.ZodOptional<z.ZodString>;
        gdrive_refresh_token: z.ZodOptional<z.ZodString>;
        gdrive_client_id: z.ZodOptional<z.ZodString>;
        gdrive_client_secret: z.ZodOptional<z.ZodString>;
        gdrive_service_account_json: z.ZodOptional<z.ZodString>;
        gdrive_folder_id: z.ZodOptional<z.ZodString>;
    }, "strip", z.ZodTypeAny, {
        mount: string;
        backend: "gdrive";
        acls?: {
            path: string;
            access: "rw" | "ro" | "deny";
        }[] | undefined;
        gdrive_access_token?: string | undefined;
        gdrive_refresh_token?: string | undefined;
        gdrive_client_id?: string | undefined;
        gdrive_client_secret?: string | undefined;
        gdrive_service_account_json?: string | undefined;
        gdrive_folder_id?: string | undefined;
    }, {
        mount: string;
        backend: "gdrive";
        acls?: {
            path: string;
            access: "rw" | "ro" | "deny";
        }[] | undefined;
        gdrive_access_token?: string | undefined;
        gdrive_refresh_token?: string | undefined;
        gdrive_client_id?: string | undefined;
        gdrive_client_secret?: string | undefined;
        gdrive_service_account_json?: string | undefined;
        gdrive_folder_id?: string | undefined;
    }>]>, "many">;
    egress: z.ZodOptional<z.ZodObject<{
        allow: z.ZodOptional<z.ZodArray<z.ZodObject<{
            host: z.ZodString;
            ports: z.ZodOptional<z.ZodArray<z.ZodNumber, "many">>;
            methods: z.ZodOptional<z.ZodArray<z.ZodEnum<["GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"]>, "many">>;
            paths: z.ZodOptional<z.ZodArray<z.ZodString, "many">>;
            headers: z.ZodOptional<z.ZodRecord<z.ZodString, z.ZodString>>;
        }, "strip", z.ZodTypeAny, {
            host: string;
            ports?: number[] | undefined;
            methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
            paths?: string[] | undefined;
            headers?: Record<string, string> | undefined;
        }, {
            host: string;
            ports?: number[] | undefined;
            methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
            paths?: string[] | undefined;
            headers?: Record<string, string> | undefined;
        }>, "many">>;
    }, "strip", z.ZodTypeAny, {
        allow?: {
            host: string;
            ports?: number[] | undefined;
            methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
            paths?: string[] | undefined;
            headers?: Record<string, string> | undefined;
        }[] | undefined;
    }, {
        allow?: {
            host: string;
            ports?: number[] | undefined;
            methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
            paths?: string[] | undefined;
            headers?: Record<string, string> | undefined;
        }[] | undefined;
    }>>;
}, "strip", z.ZodTypeAny, {
    fs: ({
        mount: string;
        backend: "local";
        acls?: {
            path: string;
            access: "rw" | "ro" | "deny";
        }[] | undefined;
    } | {
        mount: string;
        backend: "gdrive";
        acls?: {
            path: string;
            access: "rw" | "ro" | "deny";
        }[] | undefined;
        gdrive_access_token?: string | undefined;
        gdrive_refresh_token?: string | undefined;
        gdrive_client_id?: string | undefined;
        gdrive_client_secret?: string | undefined;
        gdrive_service_account_json?: string | undefined;
        gdrive_folder_id?: string | undefined;
    })[];
    image?: string | undefined;
    env?: string[] | undefined;
    ttl?: number | undefined;
    egress?: {
        allow?: {
            host: string;
            ports?: number[] | undefined;
            methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
            paths?: string[] | undefined;
            headers?: Record<string, string> | undefined;
        }[] | undefined;
    } | undefined;
}, {
    fs: ({
        mount: string;
        backend: "local";
        acls?: {
            path: string;
            access: "rw" | "ro" | "deny";
        }[] | undefined;
    } | {
        mount: string;
        backend: "gdrive";
        acls?: {
            path: string;
            access: "rw" | "ro" | "deny";
        }[] | undefined;
        gdrive_access_token?: string | undefined;
        gdrive_refresh_token?: string | undefined;
        gdrive_client_id?: string | undefined;
        gdrive_client_secret?: string | undefined;
        gdrive_service_account_json?: string | undefined;
        gdrive_folder_id?: string | undefined;
    })[];
    image?: string | undefined;
    env?: string[] | undefined;
    ttl?: number | undefined;
    egress?: {
        allow?: {
            host: string;
            ports?: number[] | undefined;
            methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
            paths?: string[] | undefined;
            headers?: Record<string, string> | undefined;
        }[] | undefined;
    } | undefined;
}>;
type SandboxConfig = z.infer<typeof SandboxConfig>;
declare const Changes: z.ZodObject<{
    fs: z.ZodOptional<z.ZodObject<{
        added: z.ZodOptional<z.ZodArray<z.ZodDiscriminatedUnion<"backend", [z.ZodObject<{
            mount: z.ZodString;
            acls: z.ZodOptional<z.ZodArray<z.ZodObject<{
                path: z.ZodString;
                access: z.ZodEnum<["rw", "ro", "deny"]>;
            }, "strip", z.ZodTypeAny, {
                path: string;
                access: "rw" | "ro" | "deny";
            }, {
                path: string;
                access: "rw" | "ro" | "deny";
            }>, "many">>;
        } & {
            backend: z.ZodLiteral<"local">;
        }, "strip", z.ZodTypeAny, {
            mount: string;
            backend: "local";
            acls?: {
                path: string;
                access: "rw" | "ro" | "deny";
            }[] | undefined;
        }, {
            mount: string;
            backend: "local";
            acls?: {
                path: string;
                access: "rw" | "ro" | "deny";
            }[] | undefined;
        }>, z.ZodObject<{
            mount: z.ZodString;
            acls: z.ZodOptional<z.ZodArray<z.ZodObject<{
                path: z.ZodString;
                access: z.ZodEnum<["rw", "ro", "deny"]>;
            }, "strip", z.ZodTypeAny, {
                path: string;
                access: "rw" | "ro" | "deny";
            }, {
                path: string;
                access: "rw" | "ro" | "deny";
            }>, "many">>;
        } & {
            backend: z.ZodLiteral<"gdrive">;
            gdrive_access_token: z.ZodOptional<z.ZodString>;
            gdrive_refresh_token: z.ZodOptional<z.ZodString>;
            gdrive_client_id: z.ZodOptional<z.ZodString>;
            gdrive_client_secret: z.ZodOptional<z.ZodString>;
            gdrive_service_account_json: z.ZodOptional<z.ZodString>;
            gdrive_folder_id: z.ZodOptional<z.ZodString>;
        }, "strip", z.ZodTypeAny, {
            mount: string;
            backend: "gdrive";
            acls?: {
                path: string;
                access: "rw" | "ro" | "deny";
            }[] | undefined;
            gdrive_access_token?: string | undefined;
            gdrive_refresh_token?: string | undefined;
            gdrive_client_id?: string | undefined;
            gdrive_client_secret?: string | undefined;
            gdrive_service_account_json?: string | undefined;
            gdrive_folder_id?: string | undefined;
        }, {
            mount: string;
            backend: "gdrive";
            acls?: {
                path: string;
                access: "rw" | "ro" | "deny";
            }[] | undefined;
            gdrive_access_token?: string | undefined;
            gdrive_refresh_token?: string | undefined;
            gdrive_client_id?: string | undefined;
            gdrive_client_secret?: string | undefined;
            gdrive_service_account_json?: string | undefined;
            gdrive_folder_id?: string | undefined;
        }>]>, "many">>;
        removed: z.ZodOptional<z.ZodArray<z.ZodDiscriminatedUnion<"backend", [z.ZodObject<{
            mount: z.ZodString;
            acls: z.ZodOptional<z.ZodArray<z.ZodObject<{
                path: z.ZodString;
                access: z.ZodEnum<["rw", "ro", "deny"]>;
            }, "strip", z.ZodTypeAny, {
                path: string;
                access: "rw" | "ro" | "deny";
            }, {
                path: string;
                access: "rw" | "ro" | "deny";
            }>, "many">>;
        } & {
            backend: z.ZodLiteral<"local">;
        }, "strip", z.ZodTypeAny, {
            mount: string;
            backend: "local";
            acls?: {
                path: string;
                access: "rw" | "ro" | "deny";
            }[] | undefined;
        }, {
            mount: string;
            backend: "local";
            acls?: {
                path: string;
                access: "rw" | "ro" | "deny";
            }[] | undefined;
        }>, z.ZodObject<{
            mount: z.ZodString;
            acls: z.ZodOptional<z.ZodArray<z.ZodObject<{
                path: z.ZodString;
                access: z.ZodEnum<["rw", "ro", "deny"]>;
            }, "strip", z.ZodTypeAny, {
                path: string;
                access: "rw" | "ro" | "deny";
            }, {
                path: string;
                access: "rw" | "ro" | "deny";
            }>, "many">>;
        } & {
            backend: z.ZodLiteral<"gdrive">;
            gdrive_access_token: z.ZodOptional<z.ZodString>;
            gdrive_refresh_token: z.ZodOptional<z.ZodString>;
            gdrive_client_id: z.ZodOptional<z.ZodString>;
            gdrive_client_secret: z.ZodOptional<z.ZodString>;
            gdrive_service_account_json: z.ZodOptional<z.ZodString>;
            gdrive_folder_id: z.ZodOptional<z.ZodString>;
        }, "strip", z.ZodTypeAny, {
            mount: string;
            backend: "gdrive";
            acls?: {
                path: string;
                access: "rw" | "ro" | "deny";
            }[] | undefined;
            gdrive_access_token?: string | undefined;
            gdrive_refresh_token?: string | undefined;
            gdrive_client_id?: string | undefined;
            gdrive_client_secret?: string | undefined;
            gdrive_service_account_json?: string | undefined;
            gdrive_folder_id?: string | undefined;
        }, {
            mount: string;
            backend: "gdrive";
            acls?: {
                path: string;
                access: "rw" | "ro" | "deny";
            }[] | undefined;
            gdrive_access_token?: string | undefined;
            gdrive_refresh_token?: string | undefined;
            gdrive_client_id?: string | undefined;
            gdrive_client_secret?: string | undefined;
            gdrive_service_account_json?: string | undefined;
            gdrive_folder_id?: string | undefined;
        }>]>, "many">>;
    }, "strip", z.ZodTypeAny, {
        added?: ({
            mount: string;
            backend: "local";
            acls?: {
                path: string;
                access: "rw" | "ro" | "deny";
            }[] | undefined;
        } | {
            mount: string;
            backend: "gdrive";
            acls?: {
                path: string;
                access: "rw" | "ro" | "deny";
            }[] | undefined;
            gdrive_access_token?: string | undefined;
            gdrive_refresh_token?: string | undefined;
            gdrive_client_id?: string | undefined;
            gdrive_client_secret?: string | undefined;
            gdrive_service_account_json?: string | undefined;
            gdrive_folder_id?: string | undefined;
        })[] | undefined;
        removed?: ({
            mount: string;
            backend: "local";
            acls?: {
                path: string;
                access: "rw" | "ro" | "deny";
            }[] | undefined;
        } | {
            mount: string;
            backend: "gdrive";
            acls?: {
                path: string;
                access: "rw" | "ro" | "deny";
            }[] | undefined;
            gdrive_access_token?: string | undefined;
            gdrive_refresh_token?: string | undefined;
            gdrive_client_id?: string | undefined;
            gdrive_client_secret?: string | undefined;
            gdrive_service_account_json?: string | undefined;
            gdrive_folder_id?: string | undefined;
        })[] | undefined;
    }, {
        added?: ({
            mount: string;
            backend: "local";
            acls?: {
                path: string;
                access: "rw" | "ro" | "deny";
            }[] | undefined;
        } | {
            mount: string;
            backend: "gdrive";
            acls?: {
                path: string;
                access: "rw" | "ro" | "deny";
            }[] | undefined;
            gdrive_access_token?: string | undefined;
            gdrive_refresh_token?: string | undefined;
            gdrive_client_id?: string | undefined;
            gdrive_client_secret?: string | undefined;
            gdrive_service_account_json?: string | undefined;
            gdrive_folder_id?: string | undefined;
        })[] | undefined;
        removed?: ({
            mount: string;
            backend: "local";
            acls?: {
                path: string;
                access: "rw" | "ro" | "deny";
            }[] | undefined;
        } | {
            mount: string;
            backend: "gdrive";
            acls?: {
                path: string;
                access: "rw" | "ro" | "deny";
            }[] | undefined;
            gdrive_access_token?: string | undefined;
            gdrive_refresh_token?: string | undefined;
            gdrive_client_id?: string | undefined;
            gdrive_client_secret?: string | undefined;
            gdrive_service_account_json?: string | undefined;
            gdrive_folder_id?: string | undefined;
        })[] | undefined;
    }>>;
    egress: z.ZodOptional<z.ZodObject<{
        added: z.ZodOptional<z.ZodArray<z.ZodObject<{
            host: z.ZodString;
            ports: z.ZodOptional<z.ZodArray<z.ZodNumber, "many">>;
            methods: z.ZodOptional<z.ZodArray<z.ZodEnum<["GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"]>, "many">>;
            paths: z.ZodOptional<z.ZodArray<z.ZodString, "many">>;
            headers: z.ZodOptional<z.ZodRecord<z.ZodString, z.ZodString>>;
        }, "strip", z.ZodTypeAny, {
            host: string;
            ports?: number[] | undefined;
            methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
            paths?: string[] | undefined;
            headers?: Record<string, string> | undefined;
        }, {
            host: string;
            ports?: number[] | undefined;
            methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
            paths?: string[] | undefined;
            headers?: Record<string, string> | undefined;
        }>, "many">>;
        removed: z.ZodOptional<z.ZodArray<z.ZodObject<{
            host: z.ZodString;
            ports: z.ZodOptional<z.ZodArray<z.ZodNumber, "many">>;
            methods: z.ZodOptional<z.ZodArray<z.ZodEnum<["GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"]>, "many">>;
            paths: z.ZodOptional<z.ZodArray<z.ZodString, "many">>;
            headers: z.ZodOptional<z.ZodRecord<z.ZodString, z.ZodString>>;
        }, "strip", z.ZodTypeAny, {
            host: string;
            ports?: number[] | undefined;
            methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
            paths?: string[] | undefined;
            headers?: Record<string, string> | undefined;
        }, {
            host: string;
            ports?: number[] | undefined;
            methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
            paths?: string[] | undefined;
            headers?: Record<string, string> | undefined;
        }>, "many">>;
    }, "strip", z.ZodTypeAny, {
        added?: {
            host: string;
            ports?: number[] | undefined;
            methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
            paths?: string[] | undefined;
            headers?: Record<string, string> | undefined;
        }[] | undefined;
        removed?: {
            host: string;
            ports?: number[] | undefined;
            methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
            paths?: string[] | undefined;
            headers?: Record<string, string> | undefined;
        }[] | undefined;
    }, {
        added?: {
            host: string;
            ports?: number[] | undefined;
            methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
            paths?: string[] | undefined;
            headers?: Record<string, string> | undefined;
        }[] | undefined;
        removed?: {
            host: string;
            ports?: number[] | undefined;
            methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
            paths?: string[] | undefined;
            headers?: Record<string, string> | undefined;
        }[] | undefined;
    }>>;
    warnings: z.ZodOptional<z.ZodArray<z.ZodString, "many">>;
}, "strip", z.ZodTypeAny, {
    fs?: {
        added?: ({
            mount: string;
            backend: "local";
            acls?: {
                path: string;
                access: "rw" | "ro" | "deny";
            }[] | undefined;
        } | {
            mount: string;
            backend: "gdrive";
            acls?: {
                path: string;
                access: "rw" | "ro" | "deny";
            }[] | undefined;
            gdrive_access_token?: string | undefined;
            gdrive_refresh_token?: string | undefined;
            gdrive_client_id?: string | undefined;
            gdrive_client_secret?: string | undefined;
            gdrive_service_account_json?: string | undefined;
            gdrive_folder_id?: string | undefined;
        })[] | undefined;
        removed?: ({
            mount: string;
            backend: "local";
            acls?: {
                path: string;
                access: "rw" | "ro" | "deny";
            }[] | undefined;
        } | {
            mount: string;
            backend: "gdrive";
            acls?: {
                path: string;
                access: "rw" | "ro" | "deny";
            }[] | undefined;
            gdrive_access_token?: string | undefined;
            gdrive_refresh_token?: string | undefined;
            gdrive_client_id?: string | undefined;
            gdrive_client_secret?: string | undefined;
            gdrive_service_account_json?: string | undefined;
            gdrive_folder_id?: string | undefined;
        })[] | undefined;
    } | undefined;
    egress?: {
        added?: {
            host: string;
            ports?: number[] | undefined;
            methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
            paths?: string[] | undefined;
            headers?: Record<string, string> | undefined;
        }[] | undefined;
        removed?: {
            host: string;
            ports?: number[] | undefined;
            methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
            paths?: string[] | undefined;
            headers?: Record<string, string> | undefined;
        }[] | undefined;
    } | undefined;
    warnings?: string[] | undefined;
}, {
    fs?: {
        added?: ({
            mount: string;
            backend: "local";
            acls?: {
                path: string;
                access: "rw" | "ro" | "deny";
            }[] | undefined;
        } | {
            mount: string;
            backend: "gdrive";
            acls?: {
                path: string;
                access: "rw" | "ro" | "deny";
            }[] | undefined;
            gdrive_access_token?: string | undefined;
            gdrive_refresh_token?: string | undefined;
            gdrive_client_id?: string | undefined;
            gdrive_client_secret?: string | undefined;
            gdrive_service_account_json?: string | undefined;
            gdrive_folder_id?: string | undefined;
        })[] | undefined;
        removed?: ({
            mount: string;
            backend: "local";
            acls?: {
                path: string;
                access: "rw" | "ro" | "deny";
            }[] | undefined;
        } | {
            mount: string;
            backend: "gdrive";
            acls?: {
                path: string;
                access: "rw" | "ro" | "deny";
            }[] | undefined;
            gdrive_access_token?: string | undefined;
            gdrive_refresh_token?: string | undefined;
            gdrive_client_id?: string | undefined;
            gdrive_client_secret?: string | undefined;
            gdrive_service_account_json?: string | undefined;
            gdrive_folder_id?: string | undefined;
        })[] | undefined;
    } | undefined;
    egress?: {
        added?: {
            host: string;
            ports?: number[] | undefined;
            methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
            paths?: string[] | undefined;
            headers?: Record<string, string> | undefined;
        }[] | undefined;
        removed?: {
            host: string;
            ports?: number[] | undefined;
            methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
            paths?: string[] | undefined;
            headers?: Record<string, string> | undefined;
        }[] | undefined;
    } | undefined;
    warnings?: string[] | undefined;
}>;
type Changes = z.infer<typeof Changes>;
declare const ApplyResult: z.ZodObject<{
    applied: z.ZodBoolean;
    config: z.ZodObject<{
        image: z.ZodOptional<z.ZodString>;
        env: z.ZodOptional<z.ZodArray<z.ZodString, "many">>;
        ttl: z.ZodOptional<z.ZodNumber>;
        fs: z.ZodArray<z.ZodDiscriminatedUnion<"backend", [z.ZodObject<{
            mount: z.ZodString;
            acls: z.ZodOptional<z.ZodArray<z.ZodObject<{
                path: z.ZodString;
                access: z.ZodEnum<["rw", "ro", "deny"]>;
            }, "strip", z.ZodTypeAny, {
                path: string;
                access: "rw" | "ro" | "deny";
            }, {
                path: string;
                access: "rw" | "ro" | "deny";
            }>, "many">>;
        } & {
            backend: z.ZodLiteral<"local">;
        }, "strip", z.ZodTypeAny, {
            mount: string;
            backend: "local";
            acls?: {
                path: string;
                access: "rw" | "ro" | "deny";
            }[] | undefined;
        }, {
            mount: string;
            backend: "local";
            acls?: {
                path: string;
                access: "rw" | "ro" | "deny";
            }[] | undefined;
        }>, z.ZodObject<{
            mount: z.ZodString;
            acls: z.ZodOptional<z.ZodArray<z.ZodObject<{
                path: z.ZodString;
                access: z.ZodEnum<["rw", "ro", "deny"]>;
            }, "strip", z.ZodTypeAny, {
                path: string;
                access: "rw" | "ro" | "deny";
            }, {
                path: string;
                access: "rw" | "ro" | "deny";
            }>, "many">>;
        } & {
            backend: z.ZodLiteral<"gdrive">;
            gdrive_access_token: z.ZodOptional<z.ZodString>;
            gdrive_refresh_token: z.ZodOptional<z.ZodString>;
            gdrive_client_id: z.ZodOptional<z.ZodString>;
            gdrive_client_secret: z.ZodOptional<z.ZodString>;
            gdrive_service_account_json: z.ZodOptional<z.ZodString>;
            gdrive_folder_id: z.ZodOptional<z.ZodString>;
        }, "strip", z.ZodTypeAny, {
            mount: string;
            backend: "gdrive";
            acls?: {
                path: string;
                access: "rw" | "ro" | "deny";
            }[] | undefined;
            gdrive_access_token?: string | undefined;
            gdrive_refresh_token?: string | undefined;
            gdrive_client_id?: string | undefined;
            gdrive_client_secret?: string | undefined;
            gdrive_service_account_json?: string | undefined;
            gdrive_folder_id?: string | undefined;
        }, {
            mount: string;
            backend: "gdrive";
            acls?: {
                path: string;
                access: "rw" | "ro" | "deny";
            }[] | undefined;
            gdrive_access_token?: string | undefined;
            gdrive_refresh_token?: string | undefined;
            gdrive_client_id?: string | undefined;
            gdrive_client_secret?: string | undefined;
            gdrive_service_account_json?: string | undefined;
            gdrive_folder_id?: string | undefined;
        }>]>, "many">;
        egress: z.ZodOptional<z.ZodObject<{
            allow: z.ZodOptional<z.ZodArray<z.ZodObject<{
                host: z.ZodString;
                ports: z.ZodOptional<z.ZodArray<z.ZodNumber, "many">>;
                methods: z.ZodOptional<z.ZodArray<z.ZodEnum<["GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"]>, "many">>;
                paths: z.ZodOptional<z.ZodArray<z.ZodString, "many">>;
                headers: z.ZodOptional<z.ZodRecord<z.ZodString, z.ZodString>>;
            }, "strip", z.ZodTypeAny, {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }, {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }>, "many">>;
        }, "strip", z.ZodTypeAny, {
            allow?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
        }, {
            allow?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
        }>>;
    }, "strip", z.ZodTypeAny, {
        fs: ({
            mount: string;
            backend: "local";
            acls?: {
                path: string;
                access: "rw" | "ro" | "deny";
            }[] | undefined;
        } | {
            mount: string;
            backend: "gdrive";
            acls?: {
                path: string;
                access: "rw" | "ro" | "deny";
            }[] | undefined;
            gdrive_access_token?: string | undefined;
            gdrive_refresh_token?: string | undefined;
            gdrive_client_id?: string | undefined;
            gdrive_client_secret?: string | undefined;
            gdrive_service_account_json?: string | undefined;
            gdrive_folder_id?: string | undefined;
        })[];
        image?: string | undefined;
        env?: string[] | undefined;
        ttl?: number | undefined;
        egress?: {
            allow?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
        } | undefined;
    }, {
        fs: ({
            mount: string;
            backend: "local";
            acls?: {
                path: string;
                access: "rw" | "ro" | "deny";
            }[] | undefined;
        } | {
            mount: string;
            backend: "gdrive";
            acls?: {
                path: string;
                access: "rw" | "ro" | "deny";
            }[] | undefined;
            gdrive_access_token?: string | undefined;
            gdrive_refresh_token?: string | undefined;
            gdrive_client_id?: string | undefined;
            gdrive_client_secret?: string | undefined;
            gdrive_service_account_json?: string | undefined;
            gdrive_folder_id?: string | undefined;
        })[];
        image?: string | undefined;
        env?: string[] | undefined;
        ttl?: number | undefined;
        egress?: {
            allow?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
        } | undefined;
    }>;
    changes: z.ZodObject<{
        fs: z.ZodOptional<z.ZodObject<{
            added: z.ZodOptional<z.ZodArray<z.ZodDiscriminatedUnion<"backend", [z.ZodObject<{
                mount: z.ZodString;
                acls: z.ZodOptional<z.ZodArray<z.ZodObject<{
                    path: z.ZodString;
                    access: z.ZodEnum<["rw", "ro", "deny"]>;
                }, "strip", z.ZodTypeAny, {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }, {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }>, "many">>;
            } & {
                backend: z.ZodLiteral<"local">;
            }, "strip", z.ZodTypeAny, {
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            }, {
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            }>, z.ZodObject<{
                mount: z.ZodString;
                acls: z.ZodOptional<z.ZodArray<z.ZodObject<{
                    path: z.ZodString;
                    access: z.ZodEnum<["rw", "ro", "deny"]>;
                }, "strip", z.ZodTypeAny, {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }, {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }>, "many">>;
            } & {
                backend: z.ZodLiteral<"gdrive">;
                gdrive_access_token: z.ZodOptional<z.ZodString>;
                gdrive_refresh_token: z.ZodOptional<z.ZodString>;
                gdrive_client_id: z.ZodOptional<z.ZodString>;
                gdrive_client_secret: z.ZodOptional<z.ZodString>;
                gdrive_service_account_json: z.ZodOptional<z.ZodString>;
                gdrive_folder_id: z.ZodOptional<z.ZodString>;
            }, "strip", z.ZodTypeAny, {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            }, {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            }>]>, "many">>;
            removed: z.ZodOptional<z.ZodArray<z.ZodDiscriminatedUnion<"backend", [z.ZodObject<{
                mount: z.ZodString;
                acls: z.ZodOptional<z.ZodArray<z.ZodObject<{
                    path: z.ZodString;
                    access: z.ZodEnum<["rw", "ro", "deny"]>;
                }, "strip", z.ZodTypeAny, {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }, {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }>, "many">>;
            } & {
                backend: z.ZodLiteral<"local">;
            }, "strip", z.ZodTypeAny, {
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            }, {
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            }>, z.ZodObject<{
                mount: z.ZodString;
                acls: z.ZodOptional<z.ZodArray<z.ZodObject<{
                    path: z.ZodString;
                    access: z.ZodEnum<["rw", "ro", "deny"]>;
                }, "strip", z.ZodTypeAny, {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }, {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }>, "many">>;
            } & {
                backend: z.ZodLiteral<"gdrive">;
                gdrive_access_token: z.ZodOptional<z.ZodString>;
                gdrive_refresh_token: z.ZodOptional<z.ZodString>;
                gdrive_client_id: z.ZodOptional<z.ZodString>;
                gdrive_client_secret: z.ZodOptional<z.ZodString>;
                gdrive_service_account_json: z.ZodOptional<z.ZodString>;
                gdrive_folder_id: z.ZodOptional<z.ZodString>;
            }, "strip", z.ZodTypeAny, {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            }, {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            }>]>, "many">>;
        }, "strip", z.ZodTypeAny, {
            added?: ({
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            } | {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            })[] | undefined;
            removed?: ({
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            } | {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            })[] | undefined;
        }, {
            added?: ({
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            } | {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            })[] | undefined;
            removed?: ({
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            } | {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            })[] | undefined;
        }>>;
        egress: z.ZodOptional<z.ZodObject<{
            added: z.ZodOptional<z.ZodArray<z.ZodObject<{
                host: z.ZodString;
                ports: z.ZodOptional<z.ZodArray<z.ZodNumber, "many">>;
                methods: z.ZodOptional<z.ZodArray<z.ZodEnum<["GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"]>, "many">>;
                paths: z.ZodOptional<z.ZodArray<z.ZodString, "many">>;
                headers: z.ZodOptional<z.ZodRecord<z.ZodString, z.ZodString>>;
            }, "strip", z.ZodTypeAny, {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }, {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }>, "many">>;
            removed: z.ZodOptional<z.ZodArray<z.ZodObject<{
                host: z.ZodString;
                ports: z.ZodOptional<z.ZodArray<z.ZodNumber, "many">>;
                methods: z.ZodOptional<z.ZodArray<z.ZodEnum<["GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"]>, "many">>;
                paths: z.ZodOptional<z.ZodArray<z.ZodString, "many">>;
                headers: z.ZodOptional<z.ZodRecord<z.ZodString, z.ZodString>>;
            }, "strip", z.ZodTypeAny, {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }, {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }>, "many">>;
        }, "strip", z.ZodTypeAny, {
            added?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
            removed?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
        }, {
            added?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
            removed?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
        }>>;
        warnings: z.ZodOptional<z.ZodArray<z.ZodString, "many">>;
    }, "strip", z.ZodTypeAny, {
        fs?: {
            added?: ({
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            } | {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            })[] | undefined;
            removed?: ({
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            } | {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            })[] | undefined;
        } | undefined;
        egress?: {
            added?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
            removed?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
        } | undefined;
        warnings?: string[] | undefined;
    }, {
        fs?: {
            added?: ({
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            } | {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            })[] | undefined;
            removed?: ({
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            } | {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            })[] | undefined;
        } | undefined;
        egress?: {
            added?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
            removed?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
        } | undefined;
        warnings?: string[] | undefined;
    }>;
    error: z.ZodOptional<z.ZodString>;
}, "strip", z.ZodTypeAny, {
    applied: boolean;
    config: {
        fs: ({
            mount: string;
            backend: "local";
            acls?: {
                path: string;
                access: "rw" | "ro" | "deny";
            }[] | undefined;
        } | {
            mount: string;
            backend: "gdrive";
            acls?: {
                path: string;
                access: "rw" | "ro" | "deny";
            }[] | undefined;
            gdrive_access_token?: string | undefined;
            gdrive_refresh_token?: string | undefined;
            gdrive_client_id?: string | undefined;
            gdrive_client_secret?: string | undefined;
            gdrive_service_account_json?: string | undefined;
            gdrive_folder_id?: string | undefined;
        })[];
        image?: string | undefined;
        env?: string[] | undefined;
        ttl?: number | undefined;
        egress?: {
            allow?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
        } | undefined;
    };
    changes: {
        fs?: {
            added?: ({
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            } | {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            })[] | undefined;
            removed?: ({
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            } | {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            })[] | undefined;
        } | undefined;
        egress?: {
            added?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
            removed?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
        } | undefined;
        warnings?: string[] | undefined;
    };
    error?: string | undefined;
}, {
    applied: boolean;
    config: {
        fs: ({
            mount: string;
            backend: "local";
            acls?: {
                path: string;
                access: "rw" | "ro" | "deny";
            }[] | undefined;
        } | {
            mount: string;
            backend: "gdrive";
            acls?: {
                path: string;
                access: "rw" | "ro" | "deny";
            }[] | undefined;
            gdrive_access_token?: string | undefined;
            gdrive_refresh_token?: string | undefined;
            gdrive_client_id?: string | undefined;
            gdrive_client_secret?: string | undefined;
            gdrive_service_account_json?: string | undefined;
            gdrive_folder_id?: string | undefined;
        })[];
        image?: string | undefined;
        env?: string[] | undefined;
        ttl?: number | undefined;
        egress?: {
            allow?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
        } | undefined;
    };
    changes: {
        fs?: {
            added?: ({
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            } | {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            })[] | undefined;
            removed?: ({
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            } | {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            })[] | undefined;
        } | undefined;
        egress?: {
            added?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
            removed?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
        } | undefined;
        warnings?: string[] | undefined;
    };
    error?: string | undefined;
}>;
type ApplyResult = z.infer<typeof ApplyResult>;
declare const ApiError: z.ZodObject<{
    error: z.ZodString;
    details: z.ZodOptional<z.ZodRecord<z.ZodString, z.ZodUnknown>>;
}, "strip", z.ZodTypeAny, {
    error: string;
    details?: Record<string, unknown> | undefined;
}, {
    error: string;
    details?: Record<string, unknown> | undefined;
}>;
type ApiError = z.infer<typeof ApiError>;
declare const SandboxRef: z.ZodObject<{
    id: z.ZodString;
    endpoint: z.ZodString;
}, "strip", z.ZodTypeAny, {
    id: string;
    endpoint: string;
}, {
    id: string;
    endpoint: string;
}>;
type SandboxRef = z.infer<typeof SandboxRef>;
declare const ConfigApplyEvent: z.ZodObject<{
    id: z.ZodNumber;
    timestamp: z.ZodString;
} & {
    type: z.ZodLiteral<"config.apply">;
    success: z.ZodBoolean;
    changes: z.ZodObject<{
        fs: z.ZodOptional<z.ZodObject<{
            added: z.ZodOptional<z.ZodArray<z.ZodDiscriminatedUnion<"backend", [z.ZodObject<{
                mount: z.ZodString;
                acls: z.ZodOptional<z.ZodArray<z.ZodObject<{
                    path: z.ZodString;
                    access: z.ZodEnum<["rw", "ro", "deny"]>;
                }, "strip", z.ZodTypeAny, {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }, {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }>, "many">>;
            } & {
                backend: z.ZodLiteral<"local">;
            }, "strip", z.ZodTypeAny, {
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            }, {
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            }>, z.ZodObject<{
                mount: z.ZodString;
                acls: z.ZodOptional<z.ZodArray<z.ZodObject<{
                    path: z.ZodString;
                    access: z.ZodEnum<["rw", "ro", "deny"]>;
                }, "strip", z.ZodTypeAny, {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }, {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }>, "many">>;
            } & {
                backend: z.ZodLiteral<"gdrive">;
                gdrive_access_token: z.ZodOptional<z.ZodString>;
                gdrive_refresh_token: z.ZodOptional<z.ZodString>;
                gdrive_client_id: z.ZodOptional<z.ZodString>;
                gdrive_client_secret: z.ZodOptional<z.ZodString>;
                gdrive_service_account_json: z.ZodOptional<z.ZodString>;
                gdrive_folder_id: z.ZodOptional<z.ZodString>;
            }, "strip", z.ZodTypeAny, {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            }, {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            }>]>, "many">>;
            removed: z.ZodOptional<z.ZodArray<z.ZodDiscriminatedUnion<"backend", [z.ZodObject<{
                mount: z.ZodString;
                acls: z.ZodOptional<z.ZodArray<z.ZodObject<{
                    path: z.ZodString;
                    access: z.ZodEnum<["rw", "ro", "deny"]>;
                }, "strip", z.ZodTypeAny, {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }, {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }>, "many">>;
            } & {
                backend: z.ZodLiteral<"local">;
            }, "strip", z.ZodTypeAny, {
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            }, {
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            }>, z.ZodObject<{
                mount: z.ZodString;
                acls: z.ZodOptional<z.ZodArray<z.ZodObject<{
                    path: z.ZodString;
                    access: z.ZodEnum<["rw", "ro", "deny"]>;
                }, "strip", z.ZodTypeAny, {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }, {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }>, "many">>;
            } & {
                backend: z.ZodLiteral<"gdrive">;
                gdrive_access_token: z.ZodOptional<z.ZodString>;
                gdrive_refresh_token: z.ZodOptional<z.ZodString>;
                gdrive_client_id: z.ZodOptional<z.ZodString>;
                gdrive_client_secret: z.ZodOptional<z.ZodString>;
                gdrive_service_account_json: z.ZodOptional<z.ZodString>;
                gdrive_folder_id: z.ZodOptional<z.ZodString>;
            }, "strip", z.ZodTypeAny, {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            }, {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            }>]>, "many">>;
        }, "strip", z.ZodTypeAny, {
            added?: ({
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            } | {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            })[] | undefined;
            removed?: ({
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            } | {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            })[] | undefined;
        }, {
            added?: ({
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            } | {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            })[] | undefined;
            removed?: ({
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            } | {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            })[] | undefined;
        }>>;
        egress: z.ZodOptional<z.ZodObject<{
            added: z.ZodOptional<z.ZodArray<z.ZodObject<{
                host: z.ZodString;
                ports: z.ZodOptional<z.ZodArray<z.ZodNumber, "many">>;
                methods: z.ZodOptional<z.ZodArray<z.ZodEnum<["GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"]>, "many">>;
                paths: z.ZodOptional<z.ZodArray<z.ZodString, "many">>;
                headers: z.ZodOptional<z.ZodRecord<z.ZodString, z.ZodString>>;
            }, "strip", z.ZodTypeAny, {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }, {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }>, "many">>;
            removed: z.ZodOptional<z.ZodArray<z.ZodObject<{
                host: z.ZodString;
                ports: z.ZodOptional<z.ZodArray<z.ZodNumber, "many">>;
                methods: z.ZodOptional<z.ZodArray<z.ZodEnum<["GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"]>, "many">>;
                paths: z.ZodOptional<z.ZodArray<z.ZodString, "many">>;
                headers: z.ZodOptional<z.ZodRecord<z.ZodString, z.ZodString>>;
            }, "strip", z.ZodTypeAny, {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }, {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }>, "many">>;
        }, "strip", z.ZodTypeAny, {
            added?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
            removed?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
        }, {
            added?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
            removed?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
        }>>;
        warnings: z.ZodOptional<z.ZodArray<z.ZodString, "many">>;
    }, "strip", z.ZodTypeAny, {
        fs?: {
            added?: ({
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            } | {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            })[] | undefined;
            removed?: ({
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            } | {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            })[] | undefined;
        } | undefined;
        egress?: {
            added?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
            removed?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
        } | undefined;
        warnings?: string[] | undefined;
    }, {
        fs?: {
            added?: ({
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            } | {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            })[] | undefined;
            removed?: ({
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            } | {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            })[] | undefined;
        } | undefined;
        egress?: {
            added?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
            removed?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
        } | undefined;
        warnings?: string[] | undefined;
    }>;
    errorMessage: z.ZodOptional<z.ZodString>;
}, "strip", z.ZodTypeAny, {
    type: "config.apply";
    changes: {
        fs?: {
            added?: ({
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            } | {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            })[] | undefined;
            removed?: ({
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            } | {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            })[] | undefined;
        } | undefined;
        egress?: {
            added?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
            removed?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
        } | undefined;
        warnings?: string[] | undefined;
    };
    id: number;
    timestamp: string;
    success: boolean;
    errorMessage?: string | undefined;
}, {
    type: "config.apply";
    changes: {
        fs?: {
            added?: ({
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            } | {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            })[] | undefined;
            removed?: ({
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            } | {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            })[] | undefined;
        } | undefined;
        egress?: {
            added?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
            removed?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
        } | undefined;
        warnings?: string[] | undefined;
    };
    id: number;
    timestamp: string;
    success: boolean;
    errorMessage?: string | undefined;
}>;
declare const EgressRequestEvent: z.ZodObject<{
    id: z.ZodNumber;
    timestamp: z.ZodString;
} & {
    type: z.ZodLiteral<"egress.request">;
    access: z.ZodEnum<["allowed", "denied"]>;
    host: z.ZodString;
    method: z.ZodEnum<["GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"]>;
    path: z.ZodString;
}, "strip", z.ZodTypeAny, {
    path: string;
    type: "egress.request";
    access: "allowed" | "denied";
    host: string;
    id: number;
    timestamp: string;
    method: "GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS";
}, {
    path: string;
    type: "egress.request";
    access: "allowed" | "denied";
    host: string;
    id: number;
    timestamp: string;
    method: "GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS";
}>;
declare const EgressResponseEvent: z.ZodObject<{
    id: z.ZodNumber;
    timestamp: z.ZodString;
} & {
    type: z.ZodLiteral<"egress.response">;
    request_id: z.ZodString;
    status: z.ZodNumber;
    duration_ms: z.ZodNumber;
}, "strip", z.ZodTypeAny, {
    type: "egress.response";
    status: number;
    id: number;
    timestamp: string;
    request_id: string;
    duration_ms: number;
}, {
    type: "egress.response";
    status: number;
    id: number;
    timestamp: string;
    request_id: string;
    duration_ms: number;
}>;
declare const FSRequestEvent: z.ZodObject<{
    id: z.ZodNumber;
    timestamp: z.ZodString;
} & {
    type: z.ZodLiteral<"fs.request">;
    access: z.ZodEnum<["allowed", "denied"]>;
    mount: z.ZodString;
    path: z.ZodString;
    operation: z.ZodEnum<["read", "write"]>;
}, "strip", z.ZodTypeAny, {
    path: string;
    type: "fs.request";
    access: "allowed" | "denied";
    mount: string;
    id: number;
    timestamp: string;
    operation: "read" | "write";
}, {
    path: string;
    type: "fs.request";
    access: "allowed" | "denied";
    mount: string;
    id: number;
    timestamp: string;
    operation: "read" | "write";
}>;
declare const FSResponseEvent: z.ZodObject<{
    id: z.ZodNumber;
    timestamp: z.ZodString;
} & {
    type: z.ZodLiteral<"fs.response">;
    backend: z.ZodEnum<["local", "gdrive"]>;
    request_id: z.ZodString;
    duration_ms: z.ZodNumber;
    error: z.ZodOptional<z.ZodString>;
}, "strip", z.ZodTypeAny, {
    type: "fs.response";
    backend: "local" | "gdrive";
    id: number;
    timestamp: string;
    request_id: string;
    duration_ms: number;
    error?: string | undefined;
}, {
    type: "fs.response";
    backend: "local" | "gdrive";
    id: number;
    timestamp: string;
    request_id: string;
    duration_ms: number;
    error?: string | undefined;
}>;
declare const StdioEvent: z.ZodObject<{
    id: z.ZodNumber;
    timestamp: z.ZodString;
} & {
    type: z.ZodLiteral<"stdio">;
    stdout: z.ZodOptional<z.ZodString>;
    stderr: z.ZodOptional<z.ZodString>;
}, "strip", z.ZodTypeAny, {
    type: "stdio";
    id: number;
    timestamp: string;
    stdout?: string | undefined;
    stderr?: string | undefined;
}, {
    type: "stdio";
    id: number;
    timestamp: string;
    stdout?: string | undefined;
    stderr?: string | undefined;
}>;
declare const SandboxEvent: z.ZodDiscriminatedUnion<"type", [z.ZodObject<{
    id: z.ZodNumber;
    timestamp: z.ZodString;
} & {
    type: z.ZodLiteral<"config.apply">;
    success: z.ZodBoolean;
    changes: z.ZodObject<{
        fs: z.ZodOptional<z.ZodObject<{
            added: z.ZodOptional<z.ZodArray<z.ZodDiscriminatedUnion<"backend", [z.ZodObject<{
                mount: z.ZodString;
                acls: z.ZodOptional<z.ZodArray<z.ZodObject<{
                    path: z.ZodString;
                    access: z.ZodEnum<["rw", "ro", "deny"]>;
                }, "strip", z.ZodTypeAny, {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }, {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }>, "many">>;
            } & {
                backend: z.ZodLiteral<"local">;
            }, "strip", z.ZodTypeAny, {
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            }, {
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            }>, z.ZodObject<{
                mount: z.ZodString;
                acls: z.ZodOptional<z.ZodArray<z.ZodObject<{
                    path: z.ZodString;
                    access: z.ZodEnum<["rw", "ro", "deny"]>;
                }, "strip", z.ZodTypeAny, {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }, {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }>, "many">>;
            } & {
                backend: z.ZodLiteral<"gdrive">;
                gdrive_access_token: z.ZodOptional<z.ZodString>;
                gdrive_refresh_token: z.ZodOptional<z.ZodString>;
                gdrive_client_id: z.ZodOptional<z.ZodString>;
                gdrive_client_secret: z.ZodOptional<z.ZodString>;
                gdrive_service_account_json: z.ZodOptional<z.ZodString>;
                gdrive_folder_id: z.ZodOptional<z.ZodString>;
            }, "strip", z.ZodTypeAny, {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            }, {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            }>]>, "many">>;
            removed: z.ZodOptional<z.ZodArray<z.ZodDiscriminatedUnion<"backend", [z.ZodObject<{
                mount: z.ZodString;
                acls: z.ZodOptional<z.ZodArray<z.ZodObject<{
                    path: z.ZodString;
                    access: z.ZodEnum<["rw", "ro", "deny"]>;
                }, "strip", z.ZodTypeAny, {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }, {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }>, "many">>;
            } & {
                backend: z.ZodLiteral<"local">;
            }, "strip", z.ZodTypeAny, {
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            }, {
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            }>, z.ZodObject<{
                mount: z.ZodString;
                acls: z.ZodOptional<z.ZodArray<z.ZodObject<{
                    path: z.ZodString;
                    access: z.ZodEnum<["rw", "ro", "deny"]>;
                }, "strip", z.ZodTypeAny, {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }, {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }>, "many">>;
            } & {
                backend: z.ZodLiteral<"gdrive">;
                gdrive_access_token: z.ZodOptional<z.ZodString>;
                gdrive_refresh_token: z.ZodOptional<z.ZodString>;
                gdrive_client_id: z.ZodOptional<z.ZodString>;
                gdrive_client_secret: z.ZodOptional<z.ZodString>;
                gdrive_service_account_json: z.ZodOptional<z.ZodString>;
                gdrive_folder_id: z.ZodOptional<z.ZodString>;
            }, "strip", z.ZodTypeAny, {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            }, {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            }>]>, "many">>;
        }, "strip", z.ZodTypeAny, {
            added?: ({
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            } | {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            })[] | undefined;
            removed?: ({
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            } | {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            })[] | undefined;
        }, {
            added?: ({
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            } | {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            })[] | undefined;
            removed?: ({
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            } | {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            })[] | undefined;
        }>>;
        egress: z.ZodOptional<z.ZodObject<{
            added: z.ZodOptional<z.ZodArray<z.ZodObject<{
                host: z.ZodString;
                ports: z.ZodOptional<z.ZodArray<z.ZodNumber, "many">>;
                methods: z.ZodOptional<z.ZodArray<z.ZodEnum<["GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"]>, "many">>;
                paths: z.ZodOptional<z.ZodArray<z.ZodString, "many">>;
                headers: z.ZodOptional<z.ZodRecord<z.ZodString, z.ZodString>>;
            }, "strip", z.ZodTypeAny, {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }, {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }>, "many">>;
            removed: z.ZodOptional<z.ZodArray<z.ZodObject<{
                host: z.ZodString;
                ports: z.ZodOptional<z.ZodArray<z.ZodNumber, "many">>;
                methods: z.ZodOptional<z.ZodArray<z.ZodEnum<["GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"]>, "many">>;
                paths: z.ZodOptional<z.ZodArray<z.ZodString, "many">>;
                headers: z.ZodOptional<z.ZodRecord<z.ZodString, z.ZodString>>;
            }, "strip", z.ZodTypeAny, {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }, {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }>, "many">>;
        }, "strip", z.ZodTypeAny, {
            added?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
            removed?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
        }, {
            added?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
            removed?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
        }>>;
        warnings: z.ZodOptional<z.ZodArray<z.ZodString, "many">>;
    }, "strip", z.ZodTypeAny, {
        fs?: {
            added?: ({
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            } | {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            })[] | undefined;
            removed?: ({
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            } | {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            })[] | undefined;
        } | undefined;
        egress?: {
            added?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
            removed?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
        } | undefined;
        warnings?: string[] | undefined;
    }, {
        fs?: {
            added?: ({
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            } | {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            })[] | undefined;
            removed?: ({
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            } | {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            })[] | undefined;
        } | undefined;
        egress?: {
            added?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
            removed?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
        } | undefined;
        warnings?: string[] | undefined;
    }>;
    errorMessage: z.ZodOptional<z.ZodString>;
}, "strip", z.ZodTypeAny, {
    type: "config.apply";
    changes: {
        fs?: {
            added?: ({
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            } | {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            })[] | undefined;
            removed?: ({
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            } | {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            })[] | undefined;
        } | undefined;
        egress?: {
            added?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
            removed?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
        } | undefined;
        warnings?: string[] | undefined;
    };
    id: number;
    timestamp: string;
    success: boolean;
    errorMessage?: string | undefined;
}, {
    type: "config.apply";
    changes: {
        fs?: {
            added?: ({
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            } | {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            })[] | undefined;
            removed?: ({
                mount: string;
                backend: "local";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
            } | {
                mount: string;
                backend: "gdrive";
                acls?: {
                    path: string;
                    access: "rw" | "ro" | "deny";
                }[] | undefined;
                gdrive_access_token?: string | undefined;
                gdrive_refresh_token?: string | undefined;
                gdrive_client_id?: string | undefined;
                gdrive_client_secret?: string | undefined;
                gdrive_service_account_json?: string | undefined;
                gdrive_folder_id?: string | undefined;
            })[] | undefined;
        } | undefined;
        egress?: {
            added?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
            removed?: {
                host: string;
                ports?: number[] | undefined;
                methods?: ("GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS")[] | undefined;
                paths?: string[] | undefined;
                headers?: Record<string, string> | undefined;
            }[] | undefined;
        } | undefined;
        warnings?: string[] | undefined;
    };
    id: number;
    timestamp: string;
    success: boolean;
    errorMessage?: string | undefined;
}>, z.ZodObject<{
    id: z.ZodNumber;
    timestamp: z.ZodString;
} & {
    type: z.ZodLiteral<"egress.request">;
    access: z.ZodEnum<["allowed", "denied"]>;
    host: z.ZodString;
    method: z.ZodEnum<["GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"]>;
    path: z.ZodString;
}, "strip", z.ZodTypeAny, {
    path: string;
    type: "egress.request";
    access: "allowed" | "denied";
    host: string;
    id: number;
    timestamp: string;
    method: "GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS";
}, {
    path: string;
    type: "egress.request";
    access: "allowed" | "denied";
    host: string;
    id: number;
    timestamp: string;
    method: "GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS";
}>, z.ZodObject<{
    id: z.ZodNumber;
    timestamp: z.ZodString;
} & {
    type: z.ZodLiteral<"egress.response">;
    request_id: z.ZodString;
    status: z.ZodNumber;
    duration_ms: z.ZodNumber;
}, "strip", z.ZodTypeAny, {
    type: "egress.response";
    status: number;
    id: number;
    timestamp: string;
    request_id: string;
    duration_ms: number;
}, {
    type: "egress.response";
    status: number;
    id: number;
    timestamp: string;
    request_id: string;
    duration_ms: number;
}>, z.ZodObject<{
    id: z.ZodNumber;
    timestamp: z.ZodString;
} & {
    type: z.ZodLiteral<"fs.request">;
    access: z.ZodEnum<["allowed", "denied"]>;
    mount: z.ZodString;
    path: z.ZodString;
    operation: z.ZodEnum<["read", "write"]>;
}, "strip", z.ZodTypeAny, {
    path: string;
    type: "fs.request";
    access: "allowed" | "denied";
    mount: string;
    id: number;
    timestamp: string;
    operation: "read" | "write";
}, {
    path: string;
    type: "fs.request";
    access: "allowed" | "denied";
    mount: string;
    id: number;
    timestamp: string;
    operation: "read" | "write";
}>, z.ZodObject<{
    id: z.ZodNumber;
    timestamp: z.ZodString;
} & {
    type: z.ZodLiteral<"fs.response">;
    backend: z.ZodEnum<["local", "gdrive"]>;
    request_id: z.ZodString;
    duration_ms: z.ZodNumber;
    error: z.ZodOptional<z.ZodString>;
}, "strip", z.ZodTypeAny, {
    type: "fs.response";
    backend: "local" | "gdrive";
    id: number;
    timestamp: string;
    request_id: string;
    duration_ms: number;
    error?: string | undefined;
}, {
    type: "fs.response";
    backend: "local" | "gdrive";
    id: number;
    timestamp: string;
    request_id: string;
    duration_ms: number;
    error?: string | undefined;
}>, z.ZodObject<{
    id: z.ZodNumber;
    timestamp: z.ZodString;
} & {
    type: z.ZodLiteral<"stdio">;
    stdout: z.ZodOptional<z.ZodString>;
    stderr: z.ZodOptional<z.ZodString>;
}, "strip", z.ZodTypeAny, {
    type: "stdio";
    id: number;
    timestamp: string;
    stdout?: string | undefined;
    stderr?: string | undefined;
}, {
    type: "stdio";
    id: number;
    timestamp: string;
    stdout?: string | undefined;
    stderr?: string | undefined;
}>]>;
type SandboxEvent = z.infer<typeof SandboxEvent>;

interface SandboxOptions {
    /** Override the global fetch (e.g. for testing or proxying). */
    fetch?: typeof fetch;
}
interface EventsStreamOptions {
    /** Resume the stream after this event id (server replays everything later). */
    lastEventId?: number;
    /** Abort the stream from the caller's side. */
    signal?: AbortSignal;
}
/**
 * A handle to a provisioned sandbox. Returned by `getOrCreateSandbox`;
 * not constructed directly by callers.
 *
 * The handle holds the per-sandbox API base URL and exposes the
 * operations against the endpoints described in `api/sandbox_server.yaml`:
 * config, ping, events, and the reverse proxy to the sandboxed
 * service.
 */
declare class Sandbox {
    readonly id: string;
    /** Base URL of the per-sandbox API server (no trailing slash). */
    readonly apiServerUrl: string;
    private readonly fetchImpl;
    constructor(ref: SandboxRef, opts?: SandboxOptions);
    /**
     * URL of the HTTP service the sandbox image exposes (the first TCP
     * port from its EXPOSE directive). Append paths to it to reach the
     * upstream — `${sandbox.getUrl()}/healthz`, etc.
     */
    getUrl(): string;
    /**
     * Reset the sandbox's TTL countdown. Bound as an arrow so
     * `setInterval(sandbox.ping, 10_000)` works without an explicit
     * `.bind(sandbox)`.
     */
    ping: () => Promise<void>;
    /** Read the current `SandboxConfig`. */
    getConfig(): Promise<SandboxConfig>;
    /**
     * Apply a desired `SandboxConfig`. Returns an `ApplyResult` whose
     * `applied` field reports whether the change was committed or
     * rolled back.
     */
    applyConfig(config: SandboxConfig): Promise<ApplyResult>;
    /**
     * Long-lived async iterator over `SandboxEvent`s. The HTTP request
     * is opened lazily on first `next()` and closes when the consumer
     * stops iterating or `signal` aborts.
     */
    getEventsStream(opts?: EventsStreamOptions): AsyncGenerator<SandboxEvent, void, void>;
    /**
     * Download a file from a sandbox mount. `path` is the agent-visible
     * absolute path (e.g. `/workspace/data.csv`). Returns the raw bytes.
     */
    downloadFile(path: string): Promise<Uint8Array>;
    /**
     * Upload `content` as a file to `destination` (which must equal one
     * of the configured `fs[].mount` paths). `filename` becomes the
     * basename written under `destination`. Returns the agent-visible
     * path and byte count the server reports.
     */
    uploadFile(destination: string, filename: string, content: Blob | Uint8Array | ArrayBuffer | string): Promise<{
        path: string;
        bytes: number;
    }>;
}
/**
 * SandboxError carries the upstream JSON `Error` payload when the
 * server returned one, so callers can switch on `err.code` /
 * `err.body.details` without re-parsing the response.
 */
declare class SandboxError extends Error {
    readonly status: number;
    readonly operation: string;
    readonly body?: {
        error: string;
        details?: Record<string, unknown>;
    };
    constructor(operation: string, status: number, message: string, body?: {
        error: string;
        details?: Record<string, unknown>;
    });
}

declare const DEFAULT_CONTROLLER_URL = "http://localhost:9000";
interface ControllerOptions {
    /** Base URL of the control plane. Defaults to `http://localhost:9000`. */
    controllerUrl?: string;
    /** Override the global fetch (e.g. for testing or custom transports). */
    fetch?: typeof fetch;
}
/**
 * Idempotent provision against `PUT /v1/sandboxes/{id}`. If a sandbox
 * with `id` already exists the controller returns it unchanged and
 * the supplied `config` is ignored; otherwise the controller creates
 * a new sandbox from `config`.
 *
 * `config` is validated against the SandboxConfig schema before the
 * request is sent — a bad config fails fast on the caller side
 * instead of producing a 400 from the controller.
 */
declare function getOrCreateSandbox(id: string, config: SandboxConfig, opts?: ControllerOptions): Promise<Sandbox>;

export { ACLRule, ApiError, ApplyResult, Backend, Changes, ConfigApplyEvent, type ControllerOptions, DEFAULT_CONTROLLER_URL, Egress, EgressRequestEvent, EgressResponseEvent, EgressRule, type EventsStreamOptions, FSRequestEvent, FSResponseEvent, FileSystem, GDriveFileSystem, HttpMethod, LocalFileSystem, Sandbox, SandboxConfig, SandboxError, SandboxEvent, type SandboxOptions, SandboxRef, StdioEvent, getOrCreateSandbox };
