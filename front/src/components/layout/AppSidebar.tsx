import { useState } from "react";
import { Link, useLocation } from "@tanstack/react-router";
import {
  Database,
  Users,
  Shield,
  Activity,
  Search,
  FileText,
  Key,
  LayoutDashboard,
  LogOut,
  Moon,
  Sun,
  LockKeyhole,
} from "lucide-react";

import { useAuth } from "@/contexts/AuthContext";
import { PasswordChangeDialog } from "@/components/shared/PasswordChangeDialog";
import { hasRole, canViewQueries, canViewAudit } from "@/lib/permissions";
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarGroupContent,
  SidebarGroupLabel,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarSeparator,
} from "@/components/ui/sidebar";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { useTheme } from "@/hooks/use-theme";

const mainNavItems = [
  { title: "Dashboard", icon: LayoutDashboard, href: "/" },
  { title: "Users", icon: Users, href: "/users" },
  { title: "Databases", icon: Database, href: "/databases" },
  { title: "Grants", icon: Shield, href: "/grants" },
];

const observabilityNavItems = [
  { title: "Connections", icon: Activity, href: "/connections" },
  { title: "Queries", icon: Search, href: "/queries" },
  { title: "Audit Log", icon: FileText, href: "/audit" },
];

const settingsNavItems = [{ title: "API Keys", icon: Key, href: "/api-keys" }];

export function AppSidebar() {
  const location = useLocation();
  const { user, logout } = useAuth();
  const { theme, setTheme } = useTheme();
  const [passwordDialogOpen, setPasswordDialogOpen] = useState(false);

  // Filter navigation items based on user roles
  const filteredMainNavItems = mainNavItems.filter((item) => {
    // Only show Users page to admins
    if (item.href === "/users") {
      return hasRole(user?.roles, "admin");
    }
    return true;
  });

  const filteredObservabilityNavItems = observabilityNavItems.filter((item) => {
    // Only show Queries and Audit Log to users with viewer role
    if (item.href === "/queries") {
      return canViewQueries(user?.roles);
    }
    if (item.href === "/audit") {
      return canViewAudit(user?.roles);
    }
    return true;
  });

  return (
    <>
    <PasswordChangeDialog
      open={passwordDialogOpen}
      onOpenChange={setPasswordDialogOpen}
    />
    <Sidebar>
      <SidebarHeader>
        <div className="flex items-center gap-2 px-2 py-1">
          <img src={`${import.meta.env.BASE_URL}logo-notext.png`} alt="DBBat" className="h-10 w-10" />
          <div className="flex flex-col">
            <span className="font-semibold text-lg text-primary">DBBat</span>
            <span className="text-xs text-muted-foreground">
              PostgreSQL Proxy
            </span>
          </div>
        </div>
      </SidebarHeader>

      <SidebarSeparator />

      <SidebarContent>
        <SidebarGroup>
          <SidebarGroupLabel>Main</SidebarGroupLabel>
          <SidebarGroupContent>
            <SidebarMenu>
              {filteredMainNavItems.map((item) => (
                <SidebarMenuItem key={item.href}>
                  <SidebarMenuButton
                    asChild
                    isActive={location.pathname === item.href}
                    tooltip={item.title}
                  >
                    <Link to={item.href}>
                      <item.icon />
                      <span>{item.title}</span>
                    </Link>
                  </SidebarMenuButton>
                </SidebarMenuItem>
              ))}
            </SidebarMenu>
          </SidebarGroupContent>
        </SidebarGroup>

        <SidebarGroup>
          <SidebarGroupLabel>Observability</SidebarGroupLabel>
          <SidebarGroupContent>
            <SidebarMenu>
              {filteredObservabilityNavItems.map((item) => (
                <SidebarMenuItem key={item.href}>
                  <SidebarMenuButton
                    asChild
                    isActive={location.pathname === item.href}
                    tooltip={item.title}
                  >
                    <Link to={item.href}>
                      <item.icon />
                      <span>{item.title}</span>
                    </Link>
                  </SidebarMenuButton>
                </SidebarMenuItem>
              ))}
            </SidebarMenu>
          </SidebarGroupContent>
        </SidebarGroup>

        <SidebarGroup>
          <SidebarGroupLabel>Settings</SidebarGroupLabel>
          <SidebarGroupContent>
            <SidebarMenu>
              {settingsNavItems.map((item) => (
                <SidebarMenuItem key={item.href}>
                  <SidebarMenuButton
                    asChild
                    isActive={location.pathname === item.href}
                    tooltip={item.title}
                  >
                    <Link to={item.href}>
                      <item.icon />
                      <span>{item.title}</span>
                    </Link>
                  </SidebarMenuButton>
                </SidebarMenuItem>
              ))}
            </SidebarMenu>
          </SidebarGroupContent>
        </SidebarGroup>
      </SidebarContent>

      <SidebarFooter>
        <SidebarMenu>
          <SidebarMenuItem>
            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <SidebarMenuButton className="w-full">
                  <div className="flex h-8 w-8 items-center justify-center rounded-full bg-primary text-primary-foreground text-sm font-medium">
                    {user?.username?.charAt(0).toUpperCase() || "U"}
                  </div>
                  <div className="flex flex-col items-start">
                    <span className="text-sm font-medium">{user?.username}</span>
                    <span className="text-xs text-muted-foreground">
                      {user?.roles?.join(", ")}
                    </span>
                  </div>
                </SidebarMenuButton>
              </DropdownMenuTrigger>
              <DropdownMenuContent side="top" className="w-56">
                <DropdownMenuItem onClick={() => setTheme(theme === "dark" ? "light" : "dark")}>
                  {theme === "dark" ? (
                    <>
                      <Sun className="mr-2 h-4 w-4" />
                      <span>Light mode</span>
                    </>
                  ) : (
                    <>
                      <Moon className="mr-2 h-4 w-4" />
                      <span>Dark mode</span>
                    </>
                  )}
                </DropdownMenuItem>
                <DropdownMenuSeparator />
                <DropdownMenuItem onClick={() => setPasswordDialogOpen(true)}>
                  <LockKeyhole className="mr-2 h-4 w-4" />
                  <span>Change password</span>
                </DropdownMenuItem>
                <DropdownMenuItem onClick={logout}>
                  <LogOut className="mr-2 h-4 w-4" />
                  <span>Log out</span>
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
          </SidebarMenuItem>
        </SidebarMenu>
      </SidebarFooter>
    </Sidebar>
    </>
  );
}
