export namespace main {
	
	export class ChatMessage {
	    role: string;
	    content: string;
	
	    static createFrom(source: any = {}) {
	        return new ChatMessage(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.role = source["role"];
	        this.content = source["content"];
	    }
	}

}

export namespace store {
	
	export class Session {
	    id: string;
	    title: string;
	    createdAt: string;
	    messageCount: number;
	
	    static createFrom(source: any = {}) {
	        return new Session(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.title = source["title"];
	        this.createdAt = source["createdAt"];
	        this.messageCount = source["messageCount"];
	    }
	}
	export class StoredMessage {
	    role: string;
	    content: string;
	    time: string;
	
	    static createFrom(source: any = {}) {
	        return new StoredMessage(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.role = source["role"];
	        this.content = source["content"];
	        this.time = source["time"];
	    }
	}

}

